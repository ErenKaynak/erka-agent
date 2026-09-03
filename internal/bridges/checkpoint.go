package bridges

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	erkaruntime "github.com/ErenKaynak/erka-agent/internal/runtime"
)

var checkpointSchema = []byte(`{
  "type": "object",
  "properties": {
    "objective": {"type": "string"},
    "constraints": {"type": "array", "items": {"type": "string"}},
    "approved_plan": {"type": "string"},
    "decisions": {"type": "array", "items": {"type": "string"}},
    "relevant_files": {"type": "array", "items": {"type": "string"}},
    "diff_summary": {"type": "string"},
    "validation_results": {"type": "array", "items": {"type": "string"}},
    "unresolved_problems": {"type": "array", "items": {"type": "string"}},
    "suggested_next_action": {"type": "string"}
  },
  "required": ["objective", "constraints", "approved_plan", "decisions", "relevant_files", "diff_summary", "validation_results", "unresolved_problems", "suggested_next_action"],
  "additionalProperties": false
}`)

func decodeCheckpoint(stage erkaruntime.Stage, text string) (erkaruntime.Checkpoint, error) {
	decoder := json.NewDecoder(bytes.NewBufferString(strings.TrimSpace(text)))
	decoder.DisallowUnknownFields()
	var checkpoint erkaruntime.Checkpoint
	if err := decoder.Decode(&checkpoint); err != nil {
		return erkaruntime.Checkpoint{}, fmt.Errorf("%w: checkpoint JSON: %v", ErrMalformedStream, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return erkaruntime.Checkpoint{}, fmt.Errorf("%w: checkpoint contains multiple JSON values", ErrMalformedStream)
	} else if !errors.Is(err, io.EOF) {
		return erkaruntime.Checkpoint{}, fmt.Errorf("%w: checkpoint trailing data: %v", ErrMalformedStream, err)
	}
	if strings.TrimSpace(checkpoint.Objective) == "" {
		return erkaruntime.Checkpoint{}, fmt.Errorf("%w: checkpoint objective is empty", ErrMalformedStream)
	}
	switch stage {
	case erkaruntime.StagePlanning:
		if strings.TrimSpace(checkpoint.ApprovedPlan) == "" {
			return erkaruntime.Checkpoint{}, fmt.Errorf("%w: planning checkpoint has no approved plan", ErrMalformedStream)
		}
	case erkaruntime.StageReview:
		if len(checkpoint.Decisions) == 0 && len(checkpoint.UnresolvedProblems) == 0 {
			return erkaruntime.Checkpoint{}, fmt.Errorf("%w: review checkpoint has no conclusion", ErrMalformedStream)
		}
	default:
		return erkaruntime.Checkpoint{}, fmt.Errorf("subscription bridges cannot run stage %q", stage)
	}
	return checkpoint, nil
}
