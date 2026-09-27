// Package dashboard is what a dashboard *is*: the definition, and the rules
// that decide whether one is worth storing.
//
// It deliberately does not store or serve anything. internal/meta keeps the
// rows and internal/api serves the HTTP; this package knows what the JSON
// means, and nothing about where it came from. That split is why the same
// validation runs for a definition POSTed by the UI and one provisioned from a
// file in an app's repo — there is one set of rules, not two that drift.
//
// # Why a definition is validated at all
//
// A dashboard is a program: every widget carries queries that will be
// evaluated later, by somebody who is not the author, at a moment when nobody
// is looking at this code. The failure mode of *not* checking is a dashboard
// that stores cleanly and then shows twelve widgets of error text — and the
// person who sees that is usually not the person who broke it.
//
// So [Dashboard.Validate] refuses a definition for things that are certain to
// fail later:
//
//   - a query that does not parse (the parser is the authority, not a regexp)
//   - a `$var` no template variable declares, which is the interesting one:
//     it cannot be caught by looking at one widget, only by comparing the
//     widgets against the dashboard around them
//   - a widget of a type this build does not have, whose queries would be
//     evaluated and then dropped on the floor
//   - a layout outside the twelve-column grid the UI draws, which would place
//     a widget where nobody can see it
//
// It does *not* refuse a query that returns nothing. An empty chart is a
// legitimate dashboard of a service that has not started yet, and a validator
// that ran queries would make saving a dashboard depend on the data being
// there — the one property a definition should not have.
//
// # Template variables
//
// A dashboard declares its variables; a widget's queries reference them with
// `$name`. Resolution happens in the evaluator, not here, because it is a
// per-request concern: the same definition is drawn for `env:prod` and
// `env:dev` by two people at once. What this package guarantees is that every
// `$name` a widget mentions is one the dashboard declares, so that resolution
// has something to resolve.
package dashboard
