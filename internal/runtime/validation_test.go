package runtime

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseValidationReport(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		output   string
		status   ValidationStatus
		failures int
		invalid  bool
	}{
		{name: "pass", output: `{"status":"pass","failures":[],"notes":"go test ok"}`, status: ValidationPass},
		{name: "fail with failures", output: `{"status":"fail","failures":["TestA","TestB"],"notes":""}`, status: ValidationFail, failures: 2},
		{name: "fail without failures", output: `{"status":"fail","failures":[],"notes":"build broken"}`, status: ValidationFail},
		{name: "surrounding whitespace", output: "\n  {\"status\":\"pass\",\"failures\":[],\"notes\":\"\"}\n", status: ValidationPass},
		{name: "json fence", output: "```json\n{\"status\":\"pass\",\"failures\":[],\"notes\":\"\"}\n```", status: ValidationPass},
		{name: "bare fence", output: "```\n{\"status\":\"fail\",\"failures\":[\"x\"],\"notes\":\"\"}\n```", status: ValidationFail, failures: 1},
		{name: "empty", output: "   ", invalid: true},
		{name: "legacy PASS prefix", output: "PASS: all tests passed", invalid: true},
		{name: "prose before json", output: `Looks good. {"status":"pass","failures":[],"notes":""}`, invalid: true},
		{name: "trailing data", output: `{"status":"pass","failures":[],"notes":""} done`, invalid: true},
		{name: "two objects", output: `{"status":"pass","failures":[],"notes":""}{"status":"pass","failures":[],"notes":""}`, invalid: true},
		{name: "uppercase status", output: `{"status":"PASS","failures":[],"notes":""}`, invalid: true},
		{name: "unknown status", output: `{"status":"ok","failures":[],"notes":""}`, invalid: true},
		{name: "missing failures", output: `{"status":"pass","notes":""}`, invalid: true},
		{name: "missing notes", output: `{"status":"pass","failures":[]}`, invalid: true},
		{name: "null failures", output: `{"status":"pass","failures":null,"notes":""}`, invalid: true},
		{name: "non-string failure", output: `{"status":"fail","failures":[1],"notes":""}`, invalid: true},
		{name: "extra property", output: `{"status":"pass","failures":[],"notes":"","score":1}`, invalid: true},
		{name: "pass listing failures", output: `{"status":"pass","failures":["TestA"],"notes":""}`, invalid: true},
		{name: "array", output: `[{"status":"pass"}]`, invalid: true},
		{name: "other fence language", output: "```go\n{\"status\":\"pass\",\"failures\":[],\"notes\":\"\"}\n```", invalid: true},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			report := ParseValidationReport(testCase.output)
			_, strictErr := DecodeValidationReport(testCase.output)
			if testCase.invalid {
				if strictErr == nil {
					t.Fatalf("DecodeValidationReport(%q) succeeded, want error", testCase.output)
				}
				if report.Status != ValidationFail || report.Passed() || len(report.Failures) != 1 {
					t.Fatalf("invalid output report = %#v, want single fail", report)
				}
				return
			}
			if strictErr != nil {
				t.Fatalf("DecodeValidationReport(%q) = %v", testCase.output, strictErr)
			}
			if report.Status != testCase.status || len(report.Failures) != testCase.failures {
				t.Fatalf("report = %#v", report)
			}
			if report.Failures == nil {
				t.Fatal("failures must be non-nil for stable JSON encoding")
			}
			if report.Passed() != (testCase.status == ValidationPass) {
				t.Fatalf("Passed() = %v for %#v", report.Passed(), report)
			}
		})
	}
}

func TestValidationReportErrorsDoNotEchoOutput(t *testing.T) {
	t.Parallel()
	const secret = "sk-live-SECRETVALUE123"
	for _, output := range []string{
		"PASS " + secret,
		`{"status":"pass","failures":[],"notes":"","` + secret + `":1}`,
		`{"status":"` + secret + `","failures":[],"notes":""}`,
		`{"status":"pass","failures":["` + secret + `"],"notes":""}`,
		`{"status":"fail","failures":[1],"notes":"` + secret + `"}`,
	} {
		report := ParseValidationReport(output)
		if report.Status != ValidationFail {
			t.Fatalf("report for %q = %#v", output, report)
		}
		if strings.Contains(report.Summary(), secret) {
			t.Fatalf("report leaks model output: %q", report.Summary())
		}
	}
}

func TestValidationReportSummaryAndJSON(t *testing.T) {
	t.Parallel()
	report := ValidationReport{Status: ValidationFail, Failures: []string{"TestA", "TestB"}, Notes: "go test ./..."}
	if got, want := report.Summary(), "fail: TestA; TestB (go test ./...)"; got != want {
		t.Fatalf("Summary() = %q, want %q", got, want)
	}
	encoded, err := json.Marshal(Checkpoint{Objective: "fix", Validation: &report})
	if err != nil {
		t.Fatal(err)
	}
	var decoded Checkpoint
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Validation == nil || decoded.Validation.Summary() != report.Summary() {
		t.Fatalf("round trip = %#v", decoded.Validation)
	}
	legacy, err := json.Marshal(Checkpoint{Objective: "fix"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(legacy), `"validation"`) {
		t.Fatalf("non-validation checkpoints must omit the field: %s", legacy)
	}
}
