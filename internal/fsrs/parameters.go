package fsrs

import (
	"errors"
	"fmt"
	"math"
	"time"
)

var ErrInvalidConfiguration = errors.New("fsrs: invalid configuration")

var DefaultParameters = [21]float64{
	0.212, 1.2931, 2.3065, 8.2956,
	6.4133, 0.8334, 3.0194, 0.001,
	1.8722, 0.1666, 0.796, 1.4835,
	0.0614, 0.2629, 1.6483, 0.6014,
	1.8729, 0.5425, 0.0912, 0.0658,
	0.1542,
}

var LowerBounds = [21]float64{
	0.001, 0.001, 0.001, 0.001,
	1.0, 0.001, 0.001, 0.001,
	0.0, 0.0, 0.001, 0.001,
	0.001, 0.001, 0.0, 0.0,
	1.0, 0.0, 0.0, 0.0,
	0.1,
}

var UpperBounds = [21]float64{
	100.0, 100.0, 100.0, 100.0,
	10.0, 4.0, 4.0, 0.75,
	4.5, 0.8, 3.5, 5.0,
	0.25, 0.9, 4.0, 1.0,
	6.0, 2.0, 2.0, 0.8,
	0.8,
}

// Config is every policy input capable of changing a server projection.
type Config struct {
	Parameters       [21]float64
	DesiredRetention float64
	LearningSteps    []time.Duration
	RelearningSteps  []time.Duration
	MaximumInterval  int
	EnableFuzzing    bool
}

// DefaultConfig returns Kelma's pinned deterministic FSRS-6 server profile.
func DefaultConfig() Config {
	return Config{
		Parameters:       DefaultParameters,
		DesiredRetention: 0.9,
		LearningSteps:    []time.Duration{time.Minute, 10 * time.Minute},
		RelearningSteps:  []time.Duration{10 * time.Minute},
		MaximumInterval:  36_500,
		EnableFuzzing:    false,
	}
}

func validateConfig(config Config) error {
	for index, value := range config.Parameters {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < LowerBounds[index] || value > UpperBounds[index] {
			return fmt.Errorf("%w: parameter[%d]=%g outside [%g,%g]", ErrInvalidConfiguration, index, value, LowerBounds[index], UpperBounds[index])
		}
	}
	if math.IsNaN(config.DesiredRetention) || math.IsInf(config.DesiredRetention, 0) || config.DesiredRetention <= 0 || config.DesiredRetention > 1 {
		return fmt.Errorf("%w: desired retention must be in (0,1]", ErrInvalidConfiguration)
	}
	if config.MaximumInterval < 1 || config.MaximumInterval > 36_500 {
		return fmt.Errorf("%w: maximum interval must be in [1,36500]", ErrInvalidConfiguration)
	}
	if config.EnableFuzzing {
		return fmt.Errorf("%w: server projections do not support fuzzing", ErrInvalidConfiguration)
	}
	for name, steps := range map[string][]time.Duration{
		"learning":   config.LearningSteps,
		"relearning": config.RelearningSteps,
	} {
		for index, step := range steps {
			if step <= 0 || step >= 24*time.Hour {
				return fmt.Errorf("%w: %s step[%d] must be between zero and one day", ErrInvalidConfiguration, name, index)
			}
			if index > 0 && step <= steps[index-1] {
				return fmt.Errorf("%w: %s steps must be strictly increasing", ErrInvalidConfiguration, name)
			}
		}
	}
	return nil
}
