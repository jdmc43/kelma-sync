package fsrs

import (
	"fmt"
	"math"
	"time"
)

// Scheduler applies one immutable FSRS-6 configuration.
type Scheduler struct {
	config Config
	algo   algorithm
}

// NewScheduler validates and copies a complete scheduler configuration.
func NewScheduler(config Config) (*Scheduler, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	config.LearningSteps = append([]time.Duration(nil), config.LearningSteps...)
	config.RelearningSteps = append([]time.Duration(nil), config.RelearningSteps...)
	return &Scheduler{config: config, algo: newAlgorithm(config.Parameters)}, nil
}

// Retrievability returns zero for an unreviewed card and the FSRS recall
// probability otherwise.
func (s *Scheduler) Retrievability(card Card, at time.Time) float64 {
	if card.LastReview == nil || card.Stability == nil {
		return 0
	}
	return s.algo.retrievability(elapsedDays(*card.LastReview, at), *card.Stability)
}

// Review updates a copy of card. The input card is not mutated.
func (s *Scheduler) ReviewCard(card Card, rating Rating, reviewedAt time.Time) (Card, error) {
	if !rating.valid() {
		return Card{}, fmt.Errorf("fsrs: invalid rating %d", rating)
	}
	if reviewedAt.IsZero() {
		return Card{}, fmt.Errorf("fsrs: review time is required")
	}

	next := cloneCard(card)
	reviewedAt = reviewedAt.UTC()
	var daysSinceLast *int
	if next.LastReview != nil {
		value := elapsedDays(*next.LastReview, reviewedAt)
		daysSinceLast = &value
	}

	var interval time.Duration
	var err error
	switch next.State {
	case Learning:
		err = s.updateLearningMemory(&next, rating, daysSinceLast, reviewedAt)
		if err == nil {
			interval, err = s.transitionLearning(&next, rating, s.config.LearningSteps)
		}
	case Review:
		err = s.updateReviewMemory(&next, rating, daysSinceLast, reviewedAt)
		if err == nil {
			interval, err = s.transitionReview(&next, rating)
		}
	case Relearning:
		err = s.updateRelearningMemory(&next, rating, daysSinceLast, reviewedAt)
		if err == nil {
			interval, err = s.transitionLearning(&next, rating, s.config.RelearningSteps)
		}
	default:
		err = fmt.Errorf("fsrs: invalid card state %d", next.State)
	}
	if err != nil {
		return Card{}, err
	}

	next.Due = reviewedAt.Add(interval)
	lastReview := reviewedAt
	next.LastReview = &lastReview
	return next, nil
}

func (s *Scheduler) updateLearningMemory(card *Card, rating Rating, daysSinceLast *int, reviewedAt time.Time) error {
	if card.Step == nil {
		return fmt.Errorf("fsrs: learning card has no step")
	}
	if card.Stability == nil || card.Difficulty == nil {
		setStability(card, s.algo.initialStability(rating))
		setDifficulty(card, s.algo.initialDifficulty(rating, true))
		return nil
	}
	return s.updateExistingMemory(card, rating, daysSinceLast, reviewedAt)
}

func (s *Scheduler) updateReviewMemory(card *Card, rating Rating, daysSinceLast *int, reviewedAt time.Time) error {
	if card.Stability == nil || card.Difficulty == nil {
		return fmt.Errorf("fsrs: review card has incomplete memory state")
	}
	return s.updateExistingMemory(card, rating, daysSinceLast, reviewedAt)
}

func (s *Scheduler) updateRelearningMemory(card *Card, rating Rating, daysSinceLast *int, reviewedAt time.Time) error {
	if card.Step == nil || card.Stability == nil || card.Difficulty == nil {
		return fmt.Errorf("fsrs: relearning card has incomplete state")
	}
	return s.updateExistingMemory(card, rating, daysSinceLast, reviewedAt)
}

func (s *Scheduler) updateExistingMemory(card *Card, rating Rating, daysSinceLast *int, reviewedAt time.Time) error {
	if card.Stability == nil || card.Difficulty == nil {
		return fmt.Errorf("fsrs: card has incomplete memory state")
	}
	stability := *card.Stability
	difficulty := *card.Difficulty
	if daysSinceLast != nil && *daysSinceLast < 1 {
		setStability(card, s.algo.shortTermStability(stability, rating))
	} else {
		retrievability := s.Retrievability(*card, reviewedAt)
		setStability(card, s.algo.nextStability(difficulty, stability, retrievability, rating))
	}
	setDifficulty(card, s.algo.nextDifficulty(difficulty, rating))
	return nil
}

func (s *Scheduler) transitionLearning(card *Card, rating Rating, steps []time.Duration) (time.Duration, error) {
	if card.Step == nil || card.Stability == nil {
		return 0, fmt.Errorf("fsrs: learning transition requires step and stability")
	}
	step := *card.Step
	if step < 0 {
		return 0, fmt.Errorf("fsrs: negative learning step")
	}
	if len(steps) == 0 || (step >= len(steps) && rating != Again) {
		return s.graduate(card), nil
	}

	switch rating {
	case Again:
		setStep(card, 0)
		return steps[0], nil
	case Hard:
		switch {
		case step == 0 && len(steps) == 1:
			return steps[0] * 3 / 2, nil
		case step == 0 && len(steps) >= 2:
			return (steps[0] + steps[1]) / 2, nil
		case step < len(steps):
			return steps[step], nil
		default:
			return 0, fmt.Errorf("fsrs: learning step %d outside %d configured steps", step, len(steps))
		}
	case Good:
		if step+1 == len(steps) {
			return s.graduate(card), nil
		}
		if step+1 > len(steps) {
			return 0, fmt.Errorf("fsrs: learning step %d outside %d configured steps", step, len(steps))
		}
		setStep(card, step+1)
		return steps[step+1], nil
	case Easy:
		return s.graduate(card), nil
	default:
		return 0, fmt.Errorf("fsrs: invalid rating %d", rating)
	}
}

func (s *Scheduler) transitionReview(card *Card, rating Rating) (time.Duration, error) {
	if card.Stability == nil {
		return 0, fmt.Errorf("fsrs: review transition requires stability")
	}
	if rating == Again && len(s.config.RelearningSteps) > 0 {
		card.State = Relearning
		setStep(card, 0)
		return s.config.RelearningSteps[0], nil
	}
	clearStep(card)
	return s.reviewInterval(*card.Stability), nil
}

func (s *Scheduler) graduate(card *Card) time.Duration {
	card.State = Review
	clearStep(card)
	return s.reviewInterval(*card.Stability)
}

func (s *Scheduler) reviewInterval(stability float64) time.Duration {
	days := s.algo.nextInterval(stability, s.config.DesiredRetention, s.config.MaximumInterval)
	return time.Duration(days) * 24 * time.Hour
}

func elapsedDays(from, to time.Time) int {
	return int(math.Floor(to.UTC().Sub(from.UTC()).Hours() / 24))
}
