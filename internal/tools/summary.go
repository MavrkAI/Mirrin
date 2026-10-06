package tools

import "context"

// Summarizer is a Tool that writes its own approval text, so the owner sees
// what the yes is really for (the whole script, the schedule and prompt, why
// a path is sensitive) instead of a clipped list of arguments.
type Summarizer interface {
	ApprovalSummary(call Call) string
}

// Checker is a Tool that can tell, before the owner is asked, that a call
// could never work (a schedule that can't run, a name already taken): the
// model hears why at once, and the owner is never asked to approve it.
type Checker interface {
	Check(ctx context.Context, call Call) error
}

// WithSummary wraps f so its approvals read summary(call).
func WithSummary(f *Func, summary func(call Call) string) Tool {
	return &summarized{Func: f, summary: summary}
}

// WithSummaryAndCheck is WithSummary for a tool whose calls check(call)
// can refuse before anyone is asked.
func WithSummaryAndCheck(f *Func, summary func(call Call) string, check func(ctx context.Context, call Call) error) Tool {
	return &summarized{Func: f, summary: summary, check: check}
}

type summarized struct {
	*Func
	summary func(call Call) string
	check   func(ctx context.Context, call Call) error
}

func (s *summarized) ApprovalSummary(call Call) string { return s.summary(call) }

func (s *summarized) Check(ctx context.Context, call Call) error {
	if s.check == nil {
		return nil
	}
	return s.check(ctx, call)
}
