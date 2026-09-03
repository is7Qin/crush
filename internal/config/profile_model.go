package config

import (
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidModelReference and ErrUnavailableModel are the stable failure
// classes for model reference parsing and resolution (spec 02). Callers
// classify with errors.Is, never with string matching.
var (
	ErrInvalidModelReference = errors.New("invalid model reference")
	ErrUnavailableModel      = errors.New("model unavailable")
)

// ParseModelRef splits an exact "<provider>/<model>" reference. The provider
// is the segment before the first slash; the model may itself contain
// slashes (e.g. "openrouter/openai/gpt-4o").
func ParseModelRef(ref string) (SelectedModel, error) {
	provider, model, found := strings.Cut(ref, "/")
	if !found || provider == "" || model == "" {
		return SelectedModel{}, fmt.Errorf("%w: %q, expected <provider>/<model>", ErrInvalidModelReference, ref)
	}
	return SelectedModel{Provider: provider, Model: model}, nil
}

// ResolveModelRef parses an exact model reference and checks it against the
// enabled provider catalog. An explicitly requested model that is
// unavailable fails; it must not silently fall back.
func (c *Config) ResolveModelRef(ref string) (SelectedModel, error) {
	model, err := ParseModelRef(ref)
	if err != nil {
		return SelectedModel{}, err
	}
	if !c.IsModelAvailable(model.Provider, model.Model) {
		return SelectedModel{}, fmt.Errorf("%w: %s", ErrUnavailableModel, ref)
	}
	return model, nil
}

// FirstAvailableModelRef returns the first reference in the ordered fallback
// list that exists in the enabled provider catalog. Unavailable entries are
// skipped; an invalid reference fails immediately and an exhausted list
// fails with ErrUnavailableModel.
func (c *Config) FirstAvailableModelRef(refs []string) (SelectedModel, error) {
	for _, ref := range refs {
		model, err := ParseModelRef(ref)
		if err != nil {
			return SelectedModel{}, err
		}
		if c.IsModelAvailable(model.Provider, model.Model) {
			return model, nil
		}
	}
	return SelectedModel{}, fmt.Errorf("%w: no candidate in %v is available", ErrUnavailableModel, refs)
}
