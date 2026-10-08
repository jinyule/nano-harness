package session

// PlanMode is the whole-value plan-mode state committed by one plan/mode
// record. The last record a session wrote itself wins; a session without one
// is not in plan mode.
type PlanMode struct {
	Active bool `json:"active"`
}

// PlanView is the plan-mode projection of a log prefix.
type PlanView struct {
	// Active is the mode the latest plan/mode committed.
	Active bool
	// Requested reports that a request/header exists; Told is the mode in
	// force when the latest one was written, which is what the model was
	// last shown.
	Requested bool
	Told      bool
}

// ProjectPlan folds the session's own plan/mode and request/header records
// into the current plan mode and the mode the latest request described.
// Records a forked child inherited from its parent are skipped: plan mode
// belongs to the session that selected it, and a child can neither select a
// mode nor pass a plan review. Events must be validated: only plan/mode
// carries Plan and only request/header carries Header.
func ProjectPlan(events []Event) PlanView {
	var view PlanView
	for _, event := range OwnEvents(events) {
		if event.Record.Plan != nil {
			view.Active = event.Record.Plan.Active
		}
		if event.Record.Header != nil {
			view.Requested, view.Told = true, view.Active
		}
	}
	return view
}

func (record Record) requirePlan() error {
	if record.Step != 0 || record.Plan == nil || record.hasExtras("plan") {
		return invalid("plan/mode shape is invalid")
	}
	return nil
}
