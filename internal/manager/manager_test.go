package manager

import (
	"errors"
	"flag"
	"io"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	corev1alpha1 "github.com/hauke-cloud/router-api/api/core/v1alpha1"
)

func TestParseFlagsDefaults(t *testing.T) {
	cfg, err := ParseFlags("core", nil, io.Discard)
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	// A manager that starts without its CRDs cannot start its informers, so
	// installing them is what happens unless someone opts out.
	if !cfg.InstallCRDs {
		t.Error("install-crds defaults to false")
	}
	if cfg.MetricsAddress != ":8080" || cfg.ProbeAddress != ":8081" || cfg.LogFormat != "json" || cfg.LogLevel != "info" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestParseFlags(t *testing.T) {
	cfg, err := ParseFlags("core", []string{"-leader-elect", "-namespace=routers", "-install-crds=false", "-log-level=debug", "-log-format=text"}, io.Discard)
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if !cfg.LeaderElection || cfg.Namespace != "routers" || cfg.InstallCRDs || cfg.LogLevel != "debug" || cfg.LogFormat != "text" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestParseFlagsRejects(t *testing.T) {
	for name, args := range map[string][]string{
		"unknown flag":   {"-nope"},
		"bad level":      {"-log-level=loud"},
		"bad format":     {"-log-format=xml"},
		"stray argument": {"serve"},
	} {
		if _, err := ParseFlags("core", args, io.Discard); err == nil {
			t.Errorf("%s: ParseFlags succeeded", name)
		}
	}
	if _, err := ParseFlags("core", []string{"-h"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h: err = %v, want flag.ErrHelp", err)
	}
}

func TestScheme(t *testing.T) {
	scheme, err := Scheme(&Definition{AddToScheme: []func(*runtime.Scheme) error{corev1alpha1.AddToScheme}})
	if err != nil {
		t.Fatalf("Scheme: %v", err)
	}
	for _, kind := range []string{"Router", "RouterDeployment"} {
		if !scheme.Recognizes(corev1alpha1.GroupVersion.WithKind(kind)) {
			t.Errorf("the scheme does not know %s", kind)
		}
	}
	// Needed to install CRDs and to read Secrets.
	if !scheme.IsVersionRegistered(corev1alpha1.GroupVersion) {
		t.Error("core group not registered")
	}
}
