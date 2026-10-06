// Package question owns the user-questions seam: validated, fail-closed
// requests for a structured answer from the local user. A tool that needs a
// decision asks the service; the selected frontend registers the broker that
// presents the questions.
package question

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/jinyule/nano-harness/internal/core/plugin"
)

const (
	// MaxQuestions bounds one request.
	MaxQuestions = 16
	// MaxOptions bounds the choices offered by one question.
	MaxOptions = 32
	// MaxIDBytes bounds one question id.
	MaxIDBytes = 128
	// MaxCustomBytes bounds one free-form answer.
	MaxCustomBytes = 16 << 10
	// IntentPlanReview marks a question whose Detail is a plan under review.
	IntentPlanReview = "plan-review"
)

var (
	// ErrNotRunning indicates the service has not started or has stopped.
	ErrNotRunning = errors.New("user-questions service is not running")
	// ErrInvalidBroker identifies a broker registration that cannot be honored.
	ErrInvalidBroker = errors.New("invalid user-questions broker registration")
	// ErrAborted reports that the caller's context ended before accepting an answer.
	ErrAborted = errors.New("ask_user_question was aborted before the user answered")
	// ErrCancelled reports that the user dismissed the questions. Brokers
	// return it for an explicit cancellation.
	ErrCancelled = errors.New("the user cancelled ask_user_question")
	// ErrDelegated reports a question from an agent owned by another agent.
	ErrDelegated = errors.New("human interaction is unavailable while the calling agent is owned by another live agent; include the unresolved question or decision in the child agent's final result")
	// ErrUnavailable reports that no broker accepted the request.
	ErrUnavailable = errors.New("no user-questions answerer accepted the request")
	// ErrInvalidAnswer reports a broker answer that does not fit the request.
	ErrInvalidAnswer = errors.New("the user-questions answerer returned an invalid answer batch")
)

// RequestError reports a request that cannot be presented. Its text is
// model-visible.
type RequestError struct{ Reason string }

func (err *RequestError) Error() string { return err.Reason }

func invalid(format string, values ...any) error {
	return &RequestError{Reason: fmt.Sprintf(format, values...)}
}

// Option is one selectable answer. Label is both the user-facing text and
// the value an answer selects.
type Option struct {
	Label       string
	Description string
}

// Intent declares that a question is a known kind of decision. It changes
// presentation only; answers use the same option labels either way.
type Intent struct {
	// Kind is IntentPlanReview, the only kind.
	Kind string
	// Approve names the option label that approves; every other answer declines.
	Approve string
}

// Question is one item of a request. ID is echoed in its answer.
type Question struct {
	ID          string
	Text        string
	Header      string
	Detail      string
	Options     []Option
	MultiSelect bool
	Intent      *Intent
}

// Request asks the user one or more questions on behalf of one tool call.
type Request struct {
	SessionID string
	CallID    string
	// Delegated marks a caller owned by another agent; it is never asked.
	Delegated bool
	Questions []Question
}

// Answer is the user's answer to one question. Selected holds option
// labels and is empty, never nil, when none was chosen. Custom is free-form
// text: it replaces the choice of a single-select question and may
// supplement a multi-select one. Empty Selected without Custom is a skip.
type Answer struct {
	ID       string
	Selected []string
	Custom   string
}

// Broker presents one request and returns one answer per question, in any
// order. It returns ErrCancelled when the user dismisses the request; any
// other failure is treated as an unavailable answerer.
type Broker interface {
	Ask(context.Context, Request) ([]Answer, error)
}

// Service validates requests and answers around the registered broker.
type Service struct {
	mu      sync.RWMutex
	started bool
	active  bool
	broker  *registration
}

// registration gives each published broker an identity, so a late cleanup
// withdraws only its own registration even when Broker values are not
// comparable.
type registration struct{ broker Broker }

// New constructs an inactive service.
func New() *Service { return &Service{} }

// ID returns the stable plugin identity.
func (*Service) ID() string { return "user-questions" }

// Start activates the service until scope cleanup withdraws every broker.
func (service *Service) Start(_ context.Context, scope *plugin.Scope) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.started {
		return ErrInvalidBroker
	}
	if err := scope.Defer(func(context.Context) error {
		service.mu.Lock()
		service.active, service.broker = false, nil
		service.mu.Unlock()
		return nil
	}); err != nil {
		return err
	}
	service.started, service.active = true, true
	return nil
}

// RegisterBroker publishes the frontend's answer surface until the caller's
// scope closes. Only one broker can be registered at a time.
func (service *Service) RegisterBroker(broker Broker, scope *plugin.Scope) error {
	if broker == nil || scope == nil {
		return ErrInvalidBroker
	}
	service.mu.Lock()
	if !service.active {
		service.mu.Unlock()
		return ErrNotRunning
	}
	if service.broker != nil {
		service.mu.Unlock()
		return ErrInvalidBroker
	}
	registered := &registration{broker: broker}
	service.broker = registered
	service.mu.Unlock()
	if err := scope.Defer(func(context.Context) error {
		service.mu.Lock()
		if service.broker == registered {
			service.broker = nil
		}
		service.mu.Unlock()
		return nil
	}); err != nil {
		service.mu.Lock()
		service.broker = nil
		service.mu.Unlock()
		return err
	}
	return nil
}

// Ask presents a validated request and returns one answer per question in
// request order. Cancellation, a delegated caller, a missing broker, a broker
// failure, and an answer that does not fit the request all fail closed.
// Cancellation is checked after the broker returns, before accepting its answers.
func (service *Service) Ask(ctx context.Context, request Request) ([]Answer, error) {
	if ctx.Err() != nil {
		return nil, ErrAborted
	}
	if len(request.Questions) == 0 {
		return nil, invalid("ask_user_question requires at least one question")
	}
	if request.Delegated {
		return nil, ErrDelegated
	}
	if err := validateQuestions(request.Questions); err != nil {
		return nil, err
	}
	service.mu.RLock()
	active, registered := service.active, service.broker
	service.mu.RUnlock()
	if !active {
		return nil, ErrNotRunning
	}
	if registered == nil {
		return nil, ErrUnavailable
	}
	answers, err := registered.broker.Ask(ctx, cloneRequest(request))
	if ctx.Err() != nil {
		return nil, ErrAborted
	}
	if err != nil {
		if errors.Is(err, ErrCancelled) {
			return nil, ErrCancelled
		}
		return nil, ErrUnavailable
	}
	return orderAnswers(request.Questions, answers)
}

func validateQuestions(questions []Question) error {
	if len(questions) > MaxQuestions {
		return invalid("ask_user_question accepts at most %d questions", MaxQuestions)
	}
	ids := map[string]struct{}{}
	for _, question := range questions {
		if question.ID == "" || len(question.ID) > MaxIDBytes || question.ID != strings.TrimSpace(question.ID) || strings.ContainsAny(question.ID, "\r\n") {
			return invalid("question ids must be trimmed single-line text of 1-%d bytes", MaxIDBytes)
		}
		if _, duplicate := ids[question.ID]; duplicate {
			return invalid("question id %q must be unique within this call", question.ID)
		}
		ids[question.ID] = struct{}{}
		if strings.TrimSpace(question.Text) == "" {
			return invalid("question %s needs non-empty question text", question.ID)
		}
		if len(question.Options) > MaxOptions {
			return invalid("question %s offers more than %d options", question.ID, MaxOptions)
		}
		labels := map[string]struct{}{}
		for _, option := range question.Options {
			if strings.TrimSpace(option.Label) == "" {
				return invalid("question %s has an option without a label", question.ID)
			}
			if _, duplicate := labels[option.Label]; duplicate {
				return invalid("question %s repeats option label %q", question.ID, option.Label)
			}
			labels[option.Label] = struct{}{}
		}
		if err := validateIntent(question, labels); err != nil {
			return err
		}
	}
	return nil
}

// validateIntent checks the two assertions an intent makes that types
// cannot: the approve label is one of the question's own options, and a
// plan review carries the plan it reviews.
func validateIntent(question Question, labels map[string]struct{}) error {
	intent := question.Intent
	if intent == nil {
		return nil
	}
	if intent.Kind != IntentPlanReview {
		return invalid("question %s declares unknown intent %q", question.ID, intent.Kind)
	}
	if _, ok := labels[intent.Approve]; !ok {
		return invalid("question %s declares intent %s whose approve label %q names none of its options", question.ID, intent.Kind, intent.Approve)
	}
	if question.Detail == "" {
		return invalid("question %s declares intent %s without the detail it reviews", question.ID, intent.Kind)
	}
	return nil
}

// orderAnswers accepts exactly one well-formed answer per question and
// returns detached answers in request order.
func orderAnswers(questions []Question, answers []Answer) ([]Answer, error) {
	if len(answers) != len(questions) {
		return nil, ErrInvalidAnswer
	}
	byID := make(map[string]Answer, len(answers))
	for _, answer := range answers {
		if _, duplicate := byID[answer.ID]; duplicate {
			return nil, ErrInvalidAnswer
		}
		byID[answer.ID] = answer
	}
	ordered := make([]Answer, len(questions))
	for index, question := range questions {
		answer, ok := byID[question.ID]
		if !ok || !validAnswer(question, answer) {
			return nil, ErrInvalidAnswer
		}
		ordered[index] = Answer{ID: answer.ID, Selected: append([]string{}, answer.Selected...), Custom: answer.Custom}
	}
	return ordered, nil
}

func validAnswer(question Question, answer Answer) bool {
	if len(answer.Custom) > MaxCustomBytes || !utf8.ValidString(answer.Custom) {
		return false
	}
	if !question.MultiSelect && (len(answer.Selected) > 1 || answer.Custom != "" && len(answer.Selected) > 0) {
		return false
	}
	seen := map[string]struct{}{}
	for _, label := range answer.Selected {
		if _, duplicate := seen[label]; duplicate {
			return false
		}
		seen[label] = struct{}{}
		if !slices.ContainsFunc(question.Options, func(option Option) bool { return option.Label == label }) {
			return false
		}
	}
	return true
}

func cloneRequest(request Request) Request {
	request.Questions = slices.Clone(request.Questions)
	for index := range request.Questions {
		request.Questions[index].Options = slices.Clone(request.Questions[index].Options)
		if intent := request.Questions[index].Intent; intent != nil {
			copied := *intent
			request.Questions[index].Intent = &copied
		}
	}
	return request
}
