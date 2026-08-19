// Package notify delivers alerts.
//
// Twelve native channels plus Apprise as a meta-provider for the long tail, and
// webhook payload templating with a preview that renders through the same code
// path as delivery — a preview that lies is worse than no preview
// (PHASE-1-PLAN.md §3.3, §3.4).
//
// The layout, because the file names alone do not say which layer is which:
//
//   - event.go     what happened, flattened so a retry describes the world as it
//     was when the event fired rather than as it is now
//   - template.go  the interpolator, and the variable catalogue the API publishes
//   - config.go    the thirteen config schemas, and the secret/public split
//   - provider.go  the shared transport, and the default rendering
//   - the rest     one file per group of providers
//   - dispatcher.go the queue, the retry policy, and the delivery log
//
// Two rules that are easy to lose under deadline:
//
//   - A channel's credentials are encrypted at rest and decrypted only at
//     delivery; a serialised notification channel never emits its secret, not
//     even to an admin (data model §12).
//   - Delivery failures are recorded, retried, and visible. An alerting system
//     that silently fails to alert is the worst failure mode this product has,
//     and it is invisible by construction unless someone builds the visibility.
package notify
