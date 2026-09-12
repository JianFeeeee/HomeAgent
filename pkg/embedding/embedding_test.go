package embedding

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

type testProvider struct {
	info   Info
	closed bool
}

func (p *testProvider) Embed(_ context.Context, _ Input) ([]float64, error) {
	return []float64{1, 0}, nil
}
func (p *testProvider) Info() Info { return p.info }
func (p *testProvider) Close()     { p.closed = true }

func TestRegistryOpensProviderWithIsolatedOptions(t *testing.T) {
	name := "test-registry-provider"
	var got Config
	Register(name, func(cfg Config) (Provider, error) {
		got = cfg
		cfg.Options["mutated"] = "inside"
		return &testProvider{info: Info{Dimension: 2, Fingerprint: "test:1"}}, nil
	})
	input := Config{Options: map[string]string{"model_dir": "/model"}}
	provider, err := Open(name, input)
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	if got.Options["model_dir"] != "/model" {
		t.Fatalf("factory options = %#v", got.Options)
	}
	if _, changed := input.Options["mutated"]; changed {
		t.Fatal("factory mutated caller-owned options")
	}
	if !reflect.DeepEqual(provider.Info(), Info{Dimension: 2, Fingerprint: "test:1"}) {
		t.Fatalf("Info = %#v", provider.Info())
	}
}

func TestOpenRejectsUnknownProvider(t *testing.T) {
	_, err := Open("definitely-missing-provider", Config{})
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("Open error = %v", err)
	}
}

func TestOpenRejectsInvalidInfoAndClosesProvider(t *testing.T) {
	name := "test-invalid-info-provider"
	provider := &testProvider{info: Info{Dimension: 0, Fingerprint: ""}}
	Register(name, func(Config) (Provider, error) { return provider, nil })
	if _, err := Open(name, Config{}); err == nil {
		t.Fatal("Open accepted invalid Info")
	}
	if !provider.closed {
		t.Fatal("invalid provider was not closed")
	}
}

func TestValidateVector(t *testing.T) {
	if err := ValidateVector([]float64{1, 2}, 2); err != nil {
		t.Fatal(err)
	}
	if err := ValidateVector([]float64{1}, 2); err == nil {
		t.Fatal("dimension mismatch accepted")
	}
	if err := ValidateVector([]float64{1, math.NaN()}, 2); err == nil {
		t.Fatal("non-finite vector accepted")
	}
}

func TestUnsupportedModalitySentinel(t *testing.T) {
	err := errors.Join(ErrUnsupportedModality, errors.New("audio"))
	if !errors.Is(err, ErrUnsupportedModality) {
		t.Fatal("sentinel does not support errors.Is")
	}
}
