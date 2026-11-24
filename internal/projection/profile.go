package projection

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	kelmafsrs "github.com/jdmc43/kelma-sync/internal/fsrs"
)

const (
	AlgorithmVersion        = "fsrs-6"
	SchedulerImplementation = "kelma-go-fsrs"
	SchedulerVersion        = "6.0.0-kelma.1"
)

var (
	ErrInvalidProfile  = errors.New("projection: invalid scheduler profile")
	ErrProfileConflict = errors.New("projection: scheduler profile conflict")
	ErrHistoryMismatch = errors.New("projection: optimization history mismatch")
)

type Profile struct {
	Version                     int64          `json:"version"`
	Algorithm                   string         `json:"algorithm"`
	SchedulerImplementation     string         `json:"scheduler_implementation"`
	SchedulerVersion            string         `json:"scheduler_version"`
	IdempotencyKey              string         `json:"idempotency_key,omitempty"`
	Parameters                  []float64      `json:"parameters"`
	ParameterSource             string         `json:"parameter_source"`
	DesiredRetention            float64        `json:"desired_retention"`
	RetentionSource             string         `json:"retention_source"`
	MaximumInterval             int            `json:"maximum_interval"`
	LearningStepsSeconds        []int          `json:"learning_steps_seconds"`
	RelearningStepsSeconds      []int          `json:"relearning_steps_seconds"`
	EnableShortTerm             bool           `json:"enable_short_term"`
	EnableFuzzing               bool           `json:"enable_fuzzing"`
	ConfigHash                  string         `json:"config_hash"`
	Optimizer                   string         `json:"optimizer,omitempty"`
	OptimizerVersion            string         `json:"optimizer_version,omitempty"`
	OptimizationHistoryHash     string         `json:"optimization_history_hash,omitempty"`
	OptimizationThroughReviewID *int64         `json:"optimization_through_review_id,omitempty"`
	QualityMetrics              map[string]any `json:"quality_metrics"`
	CreatedAt                   time.Time      `json:"created_at"`
	LastClientID                string         `json:"last_client_id,omitempty"`
}

type Candidate struct {
	BaseVersion                 int64          `json:"base_profile_version"`
	IdempotencyKey              string         `json:"idempotency_key"`
	Parameters                  []float64      `json:"parameters"`
	ParameterSource             string         `json:"parameter_source"`
	DesiredRetention            float64        `json:"desired_retention"`
	RetentionSource             string         `json:"retention_source"`
	MaximumInterval             int            `json:"maximum_interval"`
	LearningStepsSeconds        []int          `json:"learning_steps_seconds"`
	RelearningStepsSeconds      []int          `json:"relearning_steps_seconds"`
	EnableShortTerm             bool           `json:"enable_short_term"`
	EnableFuzzing               bool           `json:"enable_fuzzing"`
	Optimizer                   string         `json:"optimizer,omitempty"`
	OptimizerVersion            string         `json:"optimizer_version,omitempty"`
	OptimizationHistoryHash     string         `json:"optimization_history_hash,omitempty"`
	OptimizationThroughReviewID *int64         `json:"optimization_through_review_id,omitempty"`
	QualityMetrics              map[string]any `json:"quality_metrics,omitempty"`
}

func DefaultProfile() Profile {
	config := kelmafsrs.DefaultConfig()
	profile := Profile{
		Version:                 0,
		Algorithm:               AlgorithmVersion,
		SchedulerImplementation: SchedulerImplementation,
		SchedulerVersion:        SchedulerVersion,
		Parameters:              append([]float64(nil), config.Parameters[:]...),
		ParameterSource:         "default",
		DesiredRetention:        config.DesiredRetention,
		RetentionSource:         "default",
		MaximumInterval:         config.MaximumInterval,
		LearningStepsSeconds:    durationsToSeconds(config.LearningSteps),
		RelearningStepsSeconds:  durationsToSeconds(config.RelearningSteps),
		EnableShortTerm:         true,
		EnableFuzzing:           false,
		QualityMetrics:          map[string]any{},
	}
	profile.ConfigHash = configHash(profile)
	return profile
}

func ProfileFromCandidate(candidate Candidate) (Profile, error) {
	profile := Profile{
		Algorithm:                   AlgorithmVersion,
		SchedulerImplementation:     SchedulerImplementation,
		SchedulerVersion:            SchedulerVersion,
		IdempotencyKey:              candidate.IdempotencyKey,
		Parameters:                  append([]float64(nil), candidate.Parameters...),
		ParameterSource:             candidate.ParameterSource,
		DesiredRetention:            candidate.DesiredRetention,
		RetentionSource:             candidate.RetentionSource,
		MaximumInterval:             candidate.MaximumInterval,
		LearningStepsSeconds:        append([]int(nil), candidate.LearningStepsSeconds...),
		RelearningStepsSeconds:      append([]int(nil), candidate.RelearningStepsSeconds...),
		EnableShortTerm:             candidate.EnableShortTerm,
		EnableFuzzing:               candidate.EnableFuzzing,
		Optimizer:                   candidate.Optimizer,
		OptimizerVersion:            candidate.OptimizerVersion,
		OptimizationHistoryHash:     candidate.OptimizationHistoryHash,
		OptimizationThroughReviewID: candidate.OptimizationThroughReviewID,
		QualityMetrics:              candidate.QualityMetrics,
	}
	if profile.QualityMetrics == nil {
		profile.QualityMetrics = map[string]any{}
	}
	if err := validateProfile(profile); err != nil {
		return Profile{}, err
	}
	profile.ConfigHash = configHash(profile)
	return profile, nil
}

func (profile Profile) SchedulerConfig() (kelmafsrs.Config, error) {
	if len(profile.Parameters) != 21 {
		return kelmafsrs.Config{}, fmt.Errorf("%w: expected 21 parameters", ErrInvalidProfile)
	}
	var parameters [21]float64
	copy(parameters[:], profile.Parameters)
	config := kelmafsrs.Config{
		Parameters:       parameters,
		DesiredRetention: profile.DesiredRetention,
		MaximumInterval:  profile.MaximumInterval,
		EnableFuzzing:    profile.EnableFuzzing,
	}
	config.LearningSteps = secondsToDurations(profile.LearningStepsSeconds)
	config.RelearningSteps = secondsToDurations(profile.RelearningStepsSeconds)
	if _, err := kelmafsrs.NewScheduler(config); err != nil {
		return kelmafsrs.Config{}, fmt.Errorf("%w: %v", ErrInvalidProfile, err)
	}
	return config, nil
}

func validateProfile(profile Profile) error {
	if profile.IdempotencyKey == "" {
		return fmt.Errorf("%w: idempotency_key is required", ErrInvalidProfile)
	}
	if profile.ParameterSource != "default" && profile.ParameterSource != "client_optimized" && profile.ParameterSource != "manual" {
		return fmt.Errorf("%w: invalid parameter source", ErrInvalidProfile)
	}
	if profile.RetentionSource != "default" && profile.RetentionSource != "client_optimized" && profile.RetentionSource != "manual" {
		return fmt.Errorf("%w: invalid retention source", ErrInvalidProfile)
	}
	if profile.DesiredRetention < 0.70 || profile.DesiredRetention > 0.99 {
		return fmt.Errorf("%w: desired retention must be in [0.70,0.99]", ErrInvalidProfile)
	}
	if !profile.EnableShortTerm || profile.EnableFuzzing {
		return fmt.Errorf("%w: short-term must be enabled and fuzzing disabled", ErrInvalidProfile)
	}
	if profile.ParameterSource == "client_optimized" || profile.RetentionSource == "client_optimized" {
		if profile.Optimizer == "" || profile.OptimizerVersion == "" || profile.OptimizationHistoryHash == "" || profile.OptimizationThroughReviewID == nil {
			return fmt.Errorf("%w: optimized profiles require optimizer and history provenance", ErrInvalidProfile)
		}
	}
	if err := validateMetrics(profile.QualityMetrics); err != nil {
		return err
	}
	_, err := profile.SchedulerConfig()
	return err
}

func validateMetrics(value any) error {
	switch typed := value.(type) {
	case nil, string, bool:
		return nil
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return fmt.Errorf("%w: quality metrics must be finite", ErrInvalidProfile)
		}
		return nil
	case json.Number:
		value, err := typed.Float64()
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("%w: quality metrics must be finite numbers", ErrInvalidProfile)
		}
		return nil
	case []any:
		for _, item := range typed {
			if err := validateMetrics(item); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		for _, item := range typed {
			if err := validateMetrics(item); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%w: unsupported quality metric value", ErrInvalidProfile)
	}
}

func configHash(profile Profile) string {
	value := struct {
		Parameters             []float64 `json:"parameters"`
		DesiredRetention       float64   `json:"desired_retention"`
		MaximumInterval        int       `json:"maximum_interval"`
		LearningStepsSeconds   []int     `json:"learning_steps_seconds"`
		RelearningStepsSeconds []int     `json:"relearning_steps_seconds"`
		EnableShortTerm        bool      `json:"enable_short_term"`
		EnableFuzzing          bool      `json:"enable_fuzzing"`
	}{
		profile.Parameters, profile.DesiredRetention, profile.MaximumInterval,
		profile.LearningStepsSeconds, profile.RelearningStepsSeconds,
		profile.EnableShortTerm, profile.EnableFuzzing,
	}
	encoded, _ := json.Marshal(value)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func durationsToSeconds(values []time.Duration) []int {
	result := make([]int, len(values))
	for index, value := range values {
		result[index] = int(value / time.Second)
	}
	return result
}

func secondsToDurations(values []int) []time.Duration {
	result := make([]time.Duration, len(values))
	for index, value := range values {
		result[index] = time.Duration(value) * time.Second
	}
	return result
}
