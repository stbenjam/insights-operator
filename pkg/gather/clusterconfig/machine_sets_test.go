package clusterconfig

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestGatherMachineSet(t *testing.T) {
	var machineSetYAML = `
apiVersion: machine.openshift.io/v1beta1
kind: MachineSet
metadata:
    name: test-worker
`
	gvr := schema.GroupVersionResource{Group: "machine.openshift.io", Version: "v1beta1", Resource: "machinesets"}
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	decUnstructured := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)

	testMachineSet := &unstructured.Unstructured{}

	_, _, err := decUnstructured.Decode([]byte(machineSetYAML), nil, testMachineSet)
	if err != nil {
		t.Fatal("unable to decode machineset ", err)
	}
	_, err = client.Resource(gvr).Create(context.Background(), testMachineSet, metav1.CreateOptions{})
	if err != nil {
		t.Fatal("unable to create fake machineset ", err)
	}

	gatherer := &Gatherer{dynamicClient: client}
	records, errs := GatherMachineSet(gatherer)()
	if len(errs) > 0 {
		t.Errorf("unexpected errors: %#v", errs)
		return
	}
	if len(records) != 1 {
		t.Fatalf("unexpected number or records %d", len(records))
	}
	if records[0].Name != "machinesets/test-worker" {
		t.Fatalf("unexpected machineset name %s", records[0].Name)
	}
}
