package projection

import (
	"errors"
	"testing"

	kelmafsrs "github.com/jdmc43/kelma-sync/internal/fsrs"
)

func TestDefaultProfileMatchesPinnedSchedulerDefaults(t *testing.T) {
	t.Parallel()
	profile := DefaultProfile()
	defaults := kelmafsrs.DefaultConfig()
	if profile.Version != 0 || profile.Algorithm != AlgorithmVersion ||
		profile.DesiredRetention != 0.90 || profile.MaximumInterval != 36_500 ||
		profile.ParameterSource != "default" || profile.RetentionSource != "default" ||
		len(profile.Parameters) != 21 || profile.EnableFuzzing || !profile.EnableShortTerm ||
		profile.ConfigHash == "" {
		t.Fatalf("unexpected default profile: %+v", profile)
	}
	for index, parameter := range defaults.Parameters {
		if profile.Parameters[index] != parameter {
			t.Fatalf("parameter[%d] = %v, want %v", index, profile.Parameters[index], parameter)
		}
	}
}

func TestProfileCandidateValidation(t *testing.T) {
	t.Parallel()
	valid := validCandidate()
	if _, err := ProfileFromCandidate(valid); err != nil {
		t.Fatalf("valid candidate: %v", err)
	}

	tests := map[string]func(*Candidate){
		"missing idempotency key": func(candidate *Candidate) { candidate.IdempotencyKey = "" },
		"nineteen parameters": func(candidate *Candidate) {
			candidate.Parameters = candidate.Parameters[:19]
		},
		"retention below range": func(candidate *Candidate) { candidate.DesiredRetention = 0.69 },
		"retention above range": func(candidate *Candidate) { candidate.DesiredRetention = 1.0 },
		"fuzzing":               func(candidate *Candidate) { candidate.EnableFuzzing = true },
		"short term disabled":   func(candidate *Candidate) { candidate.EnableShortTerm = false },
		"excessive interval":    func(candidate *Candidate) { candidate.MaximumInterval = 36_501 },
		"unknown source":        func(candidate *Candidate) { candidate.ParameterSource = "server" },
		"missing optimizer provenance": func(candidate *Candidate) {
			candidate.ParameterSource = "client_optimized"
		},
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			candidate := validCandidate()
			mutate(&candidate)
			if _, err := ProfileFromCandidate(candidate); !errors.Is(err, ErrInvalidProfile) {
				t.Fatalf("error = %v, want ErrInvalidProfile", err)
			}
		})
	}
}

func TestParameterAndRetentionOwnershipAreIndependent(t *testing.T) {
	t.Parallel()
	candidate := validCandidate()
	candidate.ParameterSource = "default"
	candidate.RetentionSource = "manual"
	candidate.DesiredRetention = 0.97
	profile, err := ProfileFromCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if profile.ParameterSource != "default" || profile.RetentionSource != "manual" ||
		profile.DesiredRetention != 0.97 {
		t.Fatalf("ownership was coupled: %+v", profile)
	}
}

func validCandidate() Candidate {
	profile := DefaultProfile()
	return Candidate{
		BaseVersion: 0, IdempotencyKey: "test-request",
		Parameters: profile.Parameters, ParameterSource: "default",
		DesiredRetention: profile.DesiredRetention, RetentionSource: "default",
		MaximumInterval:        profile.MaximumInterval,
		LearningStepsSeconds:   profile.LearningStepsSeconds,
		RelearningStepsSeconds: profile.RelearningStepsSeconds,
		EnableShortTerm:        true, EnableFuzzing: false,
		QualityMetrics: map[string]any{},
	}
}
