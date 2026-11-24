package fsrs

import "math"

const minimumStability = 0.001

type algorithm struct {
	weights [21]float64
	decay   float64
	factor  float64
}

func newAlgorithm(parameters [21]float64) algorithm {
	decay := -parameters[20]
	return algorithm{
		weights: parameters,
		decay:   decay,
		factor:  math.Pow(0.9, 1/decay) - 1,
	}
}

func (a algorithm) retrievability(elapsedDays int, stability float64) float64 {
	if elapsedDays < 0 {
		elapsedDays = 0
	}
	return math.Pow(1+a.factor*float64(elapsedDays)/stability, a.decay)
}

func (a algorithm) initialStability(rating Rating) float64 {
	return math.Max(a.weights[int(rating)-1], minimumStability)
}

func (a algorithm) initialDifficulty(rating Rating, clamp bool) float64 {
	value := a.weights[4] - math.Pow(math.E, a.weights[5]*float64(rating-1)) + 1
	if clamp {
		return clampDifficulty(value)
	}
	return value
}

func (a algorithm) nextInterval(stability, desiredRetention float64, maximum int) int {
	value := stability / a.factor * (math.Pow(desiredRetention, 1/a.decay) - 1)
	interval := int(math.RoundToEven(value))
	if interval < 1 {
		interval = 1
	}
	if interval > maximum {
		interval = maximum
	}
	return interval
}

func (a algorithm) shortTermStability(stability float64, rating Rating) float64 {
	increase := math.Pow(math.E, a.weights[17]*(float64(rating)-3+a.weights[18])) *
		math.Pow(stability, -a.weights[19])
	if rating == Good || rating == Easy {
		increase = math.Max(increase, 1)
	}
	return math.Max(stability*increase, minimumStability)
}

func (a algorithm) nextDifficulty(difficulty float64, rating Rating) float64 {
	delta := -(a.weights[6] * (float64(rating) - 3))
	damped := difficulty + (10-difficulty)*delta/9
	target := a.initialDifficulty(Easy, false)
	return clampDifficulty(a.weights[7]*target + (1-a.weights[7])*damped)
}

func (a algorithm) nextStability(difficulty, stability, retrievability float64, rating Rating) float64 {
	var result float64
	if rating == Again {
		longTerm := a.weights[11] *
			math.Pow(difficulty, -a.weights[12]) *
			(math.Pow(stability+1, a.weights[13]) - 1) *
			math.Pow(math.E, (1-retrievability)*a.weights[14])
		shortTerm := stability / math.Pow(math.E, a.weights[17]*a.weights[18])
		result = math.Min(longTerm, shortTerm)
	} else {
		hardPenalty := 1.0
		if rating == Hard {
			hardPenalty = a.weights[15]
		}
		easyBonus := 1.0
		if rating == Easy {
			easyBonus = a.weights[16]
		}
		result = stability * (1 +
			math.Pow(math.E, a.weights[8])*
				(11-difficulty)*
				math.Pow(stability, -a.weights[9])*
				(math.Pow(math.E, (1-retrievability)*a.weights[10])-1)*
				hardPenalty*easyBonus)
	}
	return math.Max(result, minimumStability)
}

func clampDifficulty(value float64) float64 {
	return math.Min(math.Max(value, 1), 10)
}
