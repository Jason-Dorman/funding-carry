// Package features computes the Tier 1 to Tier 3 and risk features from venue
// state each tick, and persists the full row to cb_features.
//
// Rolling windows warm-start from database history so a restart does not reset a
// z-score.
//
// Built in Part 10.
package features
