package insightsclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/client-go/pkg/version"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"

	"k8s.io/klog/v2"

	configv1 "github.com/openshift/api/config/v1"
	configv1client "github.com/openshift/client-go/config/clientset/versioned/typed/config/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimachineryversion "k8s.io/apimachinery/pkg/version"

	"github.com/openshift/insights-operator/pkg/authorizer"
)

const (
	responseBodyLogLen = 1024
	insightsReqId      = "x-rh-insights-request-id"
	scaArchPayload     = `{"type": "sca","arch": "x86_64"}`
)

type Client struct {
	client      *http.Client
	maxBytes    int64
	metricsName string

	authorizer       Authorizer
	gatherKubeConfig *rest.Config
	clusterVersion   *configv1.ClusterVersion
}

type Authorizer interface {
	Authorize(req *http.Request) error
	NewSystemOrConfiguredProxy() func(*http.Request) (*url.URL, error)
	Token() (string, error)
}

type Source struct {
	ID           string
	Type         string
	CreationTime time.Time
	Contents     io.ReadCloser
}

// HttpError is helper error type to have HTTP error status code
type HttpError struct {
	Err        error
	StatusCode int
}

func (e HttpError) Error() string {
	return e.Err.Error()
}

func IsHttpError(err error) bool {
	switch err.(type) {
	case HttpError:
		return true
	default:
		return false
	}
}

var ErrWaitingForVersion = fmt.Errorf("waiting for the cluster version to be loaded")

// New creates a Client
func New(client *http.Client, maxBytes int64, metricsName string, authorizer Authorizer, gatherKubeConfig *rest.Config) *Client {
	if client == nil {
		client = &http.Client{}
	}
	if maxBytes == 0 {
		maxBytes = 10 * 1024 * 1024
	}
	return &Client{
		client:           client,
		maxBytes:         maxBytes,
		metricsName:      metricsName,
		authorizer:       authorizer,
		gatherKubeConfig: gatherKubeConfig,
	}
}

func getTrustedCABundle() (*x509.CertPool, error) {
	caBytes, err := os.ReadFile("/var/run/configmaps/trusted-ca-bundle/ca-bundle.crt")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(caBytes) == 0 {
		return nil, nil
	}
	certs := x509.NewCertPool()
	if ok := certs.AppendCertsFromPEM(caBytes); !ok {
		return nil, errors.New("error loading cert pool from ca data")
	}
	return certs, nil
}

// clientTransport creates new http.Transport with either system or configured Proxy
func clientTransport(authorizer Authorizer) http.RoundTripper {
	clientTransport := &http.Transport{
		Proxy: authorizer.NewSystemOrConfiguredProxy(),
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		DisableKeepAlives:   true,
	}

	// get the cluster proxy trusted CA bundle in case the proxy need it
	rootCAs, err := getTrustedCABundle()
	if err != nil {
		klog.Errorf("Failed to get proxy trusted CA: %v", err)
	}
	if rootCAs != nil {
		clientTransport.TLSClientConfig = &tls.Config{}
		clientTransport.TLSClientConfig.RootCAs = rootCAs
	}

	return transport.DebugWrappers(clientTransport)
}

func userAgent(releaseVersionEnv string, v apimachineryversion.Info, cv *configv1.ClusterVersion) string {
	gitVersion := v.GitVersion
	// If the RELEASE_VERSION is set in pod, use it
	if releaseVersionEnv != "" {
		gitVersion = releaseVersionEnv
	}
	gitVersion = fmt.Sprintf("%s-%s", gitVersion, v.GitCommit)
	return fmt.Sprintf("insights-operator/%s cluster/%s", gitVersion, cv.Spec.ClusterID)
}

func (c *Client) getClusterVersion() (*configv1.ClusterVersion, error) {
	if c.clusterVersion != nil {
		return c.clusterVersion, nil
	}
	ctx := context.Background()

	gatherConfigClient, err := configv1client.NewForConfig(c.gatherKubeConfig)
	if err != nil {
		return nil, err
	}

	cv, err := gatherConfigClient.ClusterVersions().Get(ctx, "version", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	c.clusterVersion = cv
	return cv, nil
}

func (c Client) prepareRequest(ctx context.Context, method string, endpoint string, cv *configv1.ClusterVersion) (*http.Request, error) {
	req, err := http.NewRequest(method, endpoint, nil)
	if err != nil {
		return nil, err
	}

	if req.Header == nil {
		req.Header = make(http.Header)
	}

	releaseVersionEnv := os.Getenv("RELEASE_VERSION")
	ua := userAgent(releaseVersionEnv, version.Get(), cv)
	req.Header.Set("User-Agent", ua)
	if err := c.authorizer.Authorize(req); err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	return req, nil
}

// Send uploads archives to Ingress service
func (c *Client) Send(ctx context.Context, endpoint string, source Source) error {
	cv, err := c.getClusterVersion()
	if err != nil {
		return err
	}
	if cv == nil {
		return ErrWaitingForVersion
	}

	req, err := c.prepareRequest(ctx, http.MethodPost, endpoint, cv)
	if err != nil {
		return err
	}

	bytesRead := make(chan int64, 1)
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	go c.createAndWriteMIMEHeader(&source, mw, pw, bytesRead)
	req.Body = pr
	// dynamically set the proxy environment
	c.client.Transport = clientTransport(c.authorizer)

	klog.V(4).Infof("Uploading %s to %s", source.Type, req.URL.String())
	resp, err := c.client.Do(req)
	if err != nil {
		klog.V(4).Infof("Unable to build a request, possible invalid token: %v", err)
		// if the request is not build, for example because of invalid endpoint,(maybe some problem with DNS), we want to have record about it in metrics as well.
		counterRequestSend.WithLabelValues(c.metricsName, "0").Inc()
		return fmt.Errorf("unable to build request to connect to Insights server: %v", err)
	}

	requestID := resp.Header.Get(insightsReqId)

	defer func() {
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			klog.Warningf("error copying body: %v", err)
		}
		if err := resp.Body.Close(); err != nil {
			klog.Warningf("Failed to close response body: %v", err)
		}
	}()

	counterRequestSend.WithLabelValues(c.metricsName, strconv.Itoa(resp.StatusCode)).Inc()

	if resp.StatusCode == http.StatusUnauthorized {
		klog.V(2).Infof("gateway server %s returned 401, %s=%s", resp.Request.URL, insightsReqId, requestID)
		return authorizer.Error{Err: fmt.Errorf("your Red Hat account is not enabled for remote support or your token has expired: %s", responseBody(resp))}
	}

	if resp.StatusCode == http.StatusForbidden {
		klog.V(2).Infof("gateway server %s returned 403, %s=%s", resp.Request.URL, insightsReqId, requestID)
		return authorizer.Error{Err: fmt.Errorf("your Red Hat account is not enabled for remote support")}
	}

	if resp.StatusCode == http.StatusBadRequest {
		return fmt.Errorf("gateway server bad request: %s (request=%s): %s", resp.Request.URL, requestID, responseBody(resp))
	}

	if resp.StatusCode >= 300 || resp.StatusCode < 200 {
		return fmt.Errorf("gateway server reported unexpected error code: %d (request=%s): %s", resp.StatusCode, requestID, responseBody(resp))
	}

	if len(requestID) > 0 {
		klog.V(2).Infof("Successfully reported id=%s %s=%s, wrote=%d", source.ID, insightsReqId, requestID, <-bytesRead)
	}

	return nil
}

// RecvReport perform a request to Insights Results Smart Proxy endpoint
func (c Client) RecvReport(ctx context.Context, endpoint string) (*http.Response, error) {
	cv, err := c.getClusterVersion()
	if err != nil {
		return nil, err
	}
	if cv == nil {
		return nil, ErrWaitingForVersion
	}

	endpoint = fmt.Sprintf(endpoint, cv.Spec.ClusterID)
	klog.Infof("Retrieving report for cluster: %s", cv.Spec.ClusterID)
	klog.Infof("Endpoint: %s", endpoint)

	req, err := c.prepareRequest(ctx, http.MethodGet, endpoint, cv)
	if err != nil {
		return nil, err
	}

	// dynamically set the proxy environment
	c.client.Transport = clientTransport(c.authorizer)

	klog.V(4).Infof("Retrieving report from %s", req.URL.String())
	resp, err := c.client.Do(req)

	if err != nil {
		klog.Errorf("Unable to retrieve latest report for %s: %v", cv.Spec.ClusterID, err)
		counterRequestRecvReport.WithLabelValues(c.metricsName, "0").Inc()
		return nil, err
	}

	requestID := resp.Header.Get("x-rh-insights-request-id")

	if resp.StatusCode == http.StatusUnauthorized {
		klog.V(2).Infof("gateway server %s returned 401, x-rh-insights-request-id=%s", resp.Request.URL, requestID)
		c.IncrementRecvReportMetric(resp.StatusCode)
		return nil, authorizer.Error{Err: fmt.Errorf("your Red Hat account is not enabled for remote support or your token has expired")}
	}

	if resp.StatusCode == http.StatusForbidden {
		klog.V(2).Infof("gateway server %s returned 403, x-rh-insights-request-id=%s", resp.Request.URL, requestID)
		c.IncrementRecvReportMetric(resp.StatusCode)
		return nil, authorizer.Error{Err: fmt.Errorf("your Red Hat account is not enabled for remote support")}
	}

	if resp.StatusCode == http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		if len(body) > 1024 {
			body = body[:1024]
		}
		c.IncrementRecvReportMetric(resp.StatusCode)
		return nil, fmt.Errorf("gateway server bad request: %s (request=%s): %s", resp.Request.URL, requestID, string(body))
	}
	if resp.StatusCode == http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		if len(body) > 1024 {
			body = body[:1024]
		}
		notFoundErr := HttpError{
			StatusCode: resp.StatusCode,
			Err:        fmt.Errorf("not found: %s (request=%s): %s", resp.Request.URL, requestID, string(body)),
		}
		c.IncrementRecvReportMetric(resp.StatusCode)
		return nil, notFoundErr
	}

	if resp.StatusCode >= 300 || resp.StatusCode < 200 {
		body, _ := io.ReadAll(resp.Body)
		if len(body) > 1024 {
			body = body[:1024]
		}
		c.IncrementRecvReportMetric(resp.StatusCode)
		return nil, fmt.Errorf("gateway server reported unexpected error code: %d (request=%s): %s", resp.StatusCode, requestID, string(body))
	}

	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}

	klog.Warningf("Report response status code: %d", resp.StatusCode)
	return nil, fmt.Errorf("Report response status code: %d", resp.StatusCode)
}

func (c Client) RecvSCACerts(ctx context.Context, endpoint string) ([]byte, error) {
	cv, err := c.getClusterVersion()
	if err != nil {
		return nil, err
	}
	if cv == nil {
		return nil, ErrWaitingForVersion
	}
	token, err := c.authorizer.Token()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewBuffer([]byte(scaArchPayload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.client.Transport = clientTransport(c.authorizer)
	authHeader := fmt.Sprintf("AccessToken %s:%s", cv.Spec.ClusterID, token)
	req.Header.Set("Authorization", authHeader)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve SCA certs data from %s: %v", endpoint, err)
	}

	if resp.StatusCode > 399 || resp.StatusCode < 200 {
		return nil, ocmErrorMessage(resp.Request.URL, resp)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			klog.Warningf("Failed to close response body: %v", err)
		}
	}()

	return io.ReadAll(resp.Body)
}

func responseBody(r *http.Response) string {
	if r == nil {
		return ""
	}
	body, _ := io.ReadAll(r.Body)
	if len(body) > responseBodyLogLen {
		body = body[:responseBodyLogLen]
	}
	return string(body)
}

// ocmErrorMessage wraps the OCM error states in the error
func ocmErrorMessage(url *url.URL, r *http.Response) error {
	errMessage := fmt.Sprintf("OCM API %s returned HTTP %d: %s", url, r.StatusCode, responseBody(r))
	err := fmt.Errorf(errMessage)
	return HttpError{
		Err:        err,
		StatusCode: r.StatusCode,
	}
}

// IncrementRecvReportMetric increments the "insightsclient_request_recvreport_total" metric for the given HTTP status code
func (c *Client) IncrementRecvReportMetric(statusCode int) {
	counterRequestRecvReport.WithLabelValues(c.metricsName, strconv.Itoa(statusCode)).Inc()
}

// createAndWriteMIMEHeader creates and writes a new MIME header. There are two parts (basically two content-disposition headers).
// First is to write the tar.gz file and second is to write `custom_metadata` field including gathering time info. Both parts are
// written with the provided `multipart.Writer`.
func (c *Client) createAndWriteMIMEHeader(source *Source, mw *multipart.Writer, pw *io.PipeWriter, ch chan<- int64) {
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, "file", "payload.tar.gz"))
	h.Set("Content-Type", source.Type)
	fw, err := mw.CreatePart(h)
	if err != nil {
		_ = pw.CloseWithError(err)
		return
	}
	r := &LimitedReader{R: source.Contents, N: c.maxBytes}
	n, err := io.Copy(fw, r)
	ch <- n
	if err != nil {
		_ = pw.CloseWithError(err)
	}
	// set gathering time as custom metada field
	fw, err = mw.CreateFormFile("metadata", "metadata.json")
	if err != nil {
		_ = pw.CloseWithError(err)
		return
	}
	cm := fmt.Sprintf(`{"custom_metadata":{"gathering_time":%q}}`, source.CreationTime.Format(time.RFC3339))
	_, err = io.Copy(fw, strings.NewReader(cm))
	if err != nil {
		_ = pw.CloseWithError(err)
	}
	_ = pw.CloseWithError(mw.Close())
}

var (
	counterRequestSend = metrics.NewCounterVec(&metrics.CounterOpts{
		Name: "insightsclient_request_send_total",
		Help: "Tracks the number of metrics sends",
	}, []string{"client", "status_code"})
	counterRequestRecvReport = metrics.NewCounterVec(&metrics.CounterOpts{
		Name: "insightsclient_request_recvreport_total",
		Help: "Tracks the number of reports requested",
	}, []string{"client", "status_code"})
)

func init() {
	err := legacyregistry.Register(
		counterRequestSend,
	)
	if err != nil {
		fmt.Println(err)
	}

	err = legacyregistry.Register(
		counterRequestRecvReport,
	)
	if err != nil {
		fmt.Println(err)
	}

}
