// Package testenv runs integration tests against a real kube-apiserver and
// etcd (envtest) with every CRD of router-api installed.
//
// Controllers are not started. A test calls Reconcile itself and plays the
// part of the other controllers by writing their objects, which keeps every
// test deterministic: nothing happens that the test did not ask for.
package testenv

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	configv1alpha1 "github.com/hauke-cloud/router-api/api/config/v1alpha1"
	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
	infrav1alpha1 "github.com/hauke-cloud/router-api/api/infrastructure/v1alpha1"
	"github.com/hauke-cloud/router-api/internal/crd"
)

// Scheme knows the built-in types and all three router-api groups.
func Scheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		apiextensionsv1.AddToScheme,
		corev1alpha1.AddToScheme,
		infrav1alpha1.AddToScheme,
		configv1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			panic(err)
		}
	}
	return scheme
}

// Run starts the control plane, hands a client for it to use, runs the tests
// of the package and returns their exit code. Call it from TestMain.
//
// Without KUBEBUILDER_ASSETS (set by `make test`) the tests are skipped
// rather than failed, so that a plain `go test ./...` still works.
func Run(m *testing.M, use func(client.Client)) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Println("KUBEBUILDER_ASSETS is not set, skipping integration tests; run `make test`")
		return 0
	}

	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		fmt.Println("start envtest:", err)
		return 1
	}
	defer func() { _ = env.Stop() }()

	k8s, err := client.New(cfg, client.Options{Scheme: Scheme()})
	if err != nil {
		fmt.Println("create client:", err)
		return 1
	}
	quiet := slog.New(slog.DiscardHandler)
	for _, group := range []string{
		corev1alpha1.GroupVersion.Group,
		infrav1alpha1.GroupVersion.Group,
		configv1alpha1.GroupVersion.Group,
	} {
		// The managers install their CRDs exactly like this, so every
		// integration test also proves that the embedded CRDs are valid.
		if err := crd.Install(context.Background(), k8s, group, quiet); err != nil {
			fmt.Println("install CRDs:", err)
			return 1
		}
	}

	use(k8s)
	return m.Run()
}

// Namespace creates a namespace for one test and returns its name.
func Namespace(t *testing.T, c client.Client) string {
	t.Helper()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		GenerateName: strings.ToLower(strings.NewReplacer("/", "-", "_", "-").Replace(t.Name())[:min(len(t.Name()), 40)]) + "-",
	}}
	if err := c.Create(context.Background(), namespace); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return namespace.Name
}
