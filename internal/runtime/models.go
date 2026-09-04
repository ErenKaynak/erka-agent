package runtime

import (
	"context"
	"errors"
	"strings"
	"unicode"
)

// ModelInfo is provider-neutral model metadata returned by a native provider
// or subscription bridge catalog. Catalogs never execute a model request.
type ModelInfo struct {
	ID          string
	DisplayName string
	Description string
	Default     bool
}

func (model ModelInfo) Validate() error {
	if strings.TrimSpace(model.ID) == "" {
		return errors.New("model ID is empty")
	}
	if len(model.ID) > 512 || strings.ContainsFunc(model.ID, unicode.IsControl) {
		return errors.New("model ID contains unsupported control characters")
	}
	return nil
}

// ModelCatalog lists models available through one authenticated execution
// backend without making a paid inference request.
type ModelCatalog interface {
	ListModels(context.Context) ([]ModelInfo, error)
}

// ModelSelection is a session-only route override for one pipeline stage.
type ModelSelection struct {
	Stage      Stage
	Connection string
	Model      string
}
