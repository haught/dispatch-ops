package crd

import (
	"context"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// installed is the CRD as an older chart or operator left it, applied by
// the operator version applied ("" for none).
func installed(applied string) *apiextensionsv1.CustomResourceDefinition {
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: Name, Annotations: map[string]string{"keep": "me"}},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "dispatch-ops.io",
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "dispatchops", Kind: "DispatchOps"},
			Scope: apiextensionsv1.NamespaceScoped,
		},
	}
	if applied != "" {
		crd.Annotations[VersionAnnotation] = applied
	}
	return crd
}

func get(t *testing.T, c client.Client) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	var crd apiextensionsv1.CustomResourceDefinition
	if err := c.Get(context.Background(), types.NamespacedName{Name: Name}, &crd); err != nil {
		t.Fatal(err)
	}
	return &crd
}

func TestEnsure(t *testing.T) {
	want, err := Manifest()
	if err != nil {
		t.Fatal(err)
	}
	wantVersions, _, _ := unstructured.NestedSlice(want.Object, "spec", "versions")
	for _, tc := range []struct {
		name, applied, version string
		applies                bool
	}{
		{name: "chart's CRD", version: "1.1.0", applies: true},
		{name: "older operator", applied: "1.0.0", version: "1.1.0", applies: true},
		{name: "same version", applied: "v1.1.0", version: "1.1.0", applies: true},
		{name: "newer operator", applied: "1.2.0", version: "1.1.0"},
		{name: "after a development build", applied: "dev", version: "1.0.0", applies: true},
		{name: "development build", applied: "9.0.0", version: "dev", applies: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, installed(tc.applied))
			if err := Ensure(context.Background(), c, tc.version); err != nil {
				t.Fatal(err)
			}
			got := get(t, c)
			applied := got.Annotations[VersionAnnotation] == tc.version
			if applied != tc.applies {
				t.Fatalf("version annotation %q; applied %v, want %v", got.Annotations[VersionAnnotation], applied, tc.applies)
			}
			if tc.applies && len(got.Spec.Versions) != len(wantVersions) {
				t.Errorf("%d versions, want %d", len(got.Spec.Versions), len(wantVersions))
			}
		})
	}
}

func TestEnsureMissing(t *testing.T) {
	err := Ensure(context.Background(), newClient(t), "1.0.0")
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("error %v, want not installed", err)
	}
}
