// Package status serves public status pages.
//
// Unauthenticated, cacheable, custom-domain capable, and the one surface where a
// stranger judges the product in five seconds. It reads the same rollups the
// dashboard does and must respect the same rule: a bucket with no observations
// renders as "no data", never as downtime. unknown and skipped are gaps, not
// outages, and rendering a probe failure as customer downtime is a lie that
// Phase 2's SLA reports would inherit (data model §5.3, ADR-005 decision 16).
package status
