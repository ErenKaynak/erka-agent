package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ValidationStatus is the machine-readable verdict of a validation stage.
type ValidationStatus string

const (
	ValidationPass ValidationStatus = "pass"
	ValidationFail ValidationStatus = "fail"
)

// ValidationReport is the structured result a validation stage must return.
type ValidationReport struct {
	Status   ValidationStatus `json:"status"`
	Failures []string         `json:"failures"`
	Notes    string           `json:"notes"`
}

// Passed reports whether the report is a well-formed pass verdict.
func (report *ValidationReport) Passed() bool {
	return report != nil && report.Status == ValidationPass && len(report.Failures) == 0
}

// Summary renders the report as one human-readable line for checkpoint
// summaries and UIs that predate structured validation.
func (report ValidationReport) Summary() string {
	var builder strings.Builder
	builder.WriteString(string(report.Status))
	if len(report.Failures) > 0 {
		builder.WriteString(": ")
		builder.WriteString(strings.Join(report.Failures, "; "))
	}
	if notes := strings.TrimSpace(report.Notes); notes != "" {
		builder.WriteString(" (")
		builder.WriteString(notes)
		builder.WriteString(")")
	}
	return builder.String()
}

// ValidationReportSchema is the JSON Schema the validation stage's final
// response must satisfy.
const ValidationReportSchema = `{
  "type": "object",
  "properties": {
    "status": {"enum": ["pass", "fail"]},
    "failures": {"type": "array", "items": {"type": "string"}},
    "notes": {"type": "string"}
  },
  "required": ["status", "failures", "notes"],
  "additionalProperties": false
}`

var (
	validationSchemaOnce sync.Once
	validationSchema     *jsonschema.Schema
	validationSchemaErr  error
)

func compiledValidationSchema() (*jsonschema.Schema, error) {
	validationSchemaOnce.Do(func() {
		document, err := jsonschema.UnmarshalJSON(strings.NewReader(ValidationReportSchema))
		if err != nil {
			validationSchemaErr = err
			return
		}
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource("validation.json", document); err != nil {
			validationSchemaErr = err
			return
		}
		validationSchema, validationSchemaErr = compiler.Compile("validation.json")
	})
	return validationSchema, validationSchemaErr
}

// ParseValidationReport decodes a validation stage's final response. The
// response must be a single JSON object matching ValidationReportSchema,
// optionally wrapped in one Markdown code fence. Any other output yields a
// fail report; the failure reason never echoes the model output.
func ParseValidationReport(output string) ValidationReport {
	report, err := DecodeValidationReport(output)
	if err != nil {
		return invalidValidationReport(err)
	}
	return report
}

func invalidValidationReport(err error) ValidationReport {
	return ValidationReport{
		Status:   ValidationFail,
		Failures: []string{"validation output did not match the required schema: " + err.Error()},
	}
}

var errValidationEmpty = errors.New("output is empty")

// DecodeValidationReport is the strict form of ParseValidationReport: it
// returns an error instead of a fail report. Errors never echo the output.
func DecodeValidationReport(output string) (ValidationReport, error) {
	payload := stripCodeFence(strings.TrimSpace(output))
	if payload == "" {
		return ValidationReport{}, errValidationEmpty
	}
	value, err := decodeSingleJSON(payload)
	if err != nil {
		return ValidationReport{}, err
	}
	schema, err := compiledValidationSchema()
	if err != nil {
		return ValidationReport{}, fmt.Errorf("schema unavailable: %v", err)
	}
	if err := schema.Validate(value); err != nil {
		return ValidationReport{}, errors.New(describeSchemaError(err))
	}
	var report ValidationReport
	if err := json.Unmarshal([]byte(payload), &report); err != nil {
		return ValidationReport{}, errors.New("output is not a validation report")
	}
	if report.Failures == nil {
		report.Failures = []string{}
	}
	if report.Status == ValidationPass && len(report.Failures) > 0 {
		return ValidationReport{}, errors.New("status pass must not list failures")
	}
	return report, nil
}

// decodeSingleJSON accepts exactly one JSON value with no trailing data.
func decodeSingleJSON(payload string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, errors.New("output is not valid JSON")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("output contains data after the JSON object")
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, errors.New("output is not a JSON object")
	}
	return value, nil
}

// stripCodeFence removes one surrounding ``` or ```json fence, if present.
func stripCodeFence(text string) string {
	if !strings.HasPrefix(text, "```") || !strings.HasSuffix(text, "```") || len(text) < 6 {
		return text
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(text, "```"), "```")
	newline := strings.IndexByte(inner, '\n')
	if newline < 0 {
		return text
	}
	language := strings.TrimSpace(inner[:newline])
	if language != "" && !strings.EqualFold(language, "json") {
		return text
	}
	body := inner[newline+1:]
	if strings.Contains(body, "```") {
		return text
	}
	return strings.TrimSpace(body)
}

// describeSchemaError reports which field failed and why, using only schema
// locations and keywords so that model output is never echoed.
func describeSchemaError(err error) string {
	var validation *jsonschema.ValidationError
	if !errors.As(err, &validation) {
		return "output does not match the validation schema"
	}
	leaf := validation
	for len(leaf.Causes) > 0 {
		leaf = leaf.Causes[0]
	}
	location := "/" + strings.Join(leaf.InstanceLocation, "/")
	keyword := "schema"
	if path := leaf.ErrorKind.KeywordPath(); len(path) > 0 {
		keyword = path[len(path)-1]
	}
	return fmt.Sprintf("field %s violates %q", location, keyword)
}
