# Internal doc: Consensus policy

## Overview

Consensus collects responses from multiple upstreams, groups identical outcomes, and selects a winner using threshold-based agreement, explicit preferences, and tie/dispute handling. It supports network faults, execution errors, and block-head leader preferences.

## Inputs and grouping
- Group responses by canonical hash (results) or normalized error code.
- Response types: NonEmpty, Empty/Emptyish, ConsensusError (JSON-RPC client/missing/execution), InfrastructureError.
- Track: count per group, first result/error, response size (bytes), validParticipants (non-infra).
- Optional IgnoreFields may be applied to the canonical hash per method.

## Low-participants vs dispute
- Low participants: validParticipants < AgreementThreshold.
- Otherwise we’re in the normal/dispute path until a threshold winner emerges.

## Block-head leader behaviors
- OnlyBlockHeadLeader
  - Dispute: if no valid group meets threshold and the leader has a non-error result, return it; else if the leader only has an error, return that error; else dispute.
  - Low participants: same pattern; if no leader non-error and no leader error, return LowParticipants error.
- PreferBlockHeadLeader
  - Dispute: when no valid group meets threshold, if leader has a non-error result, return it; else fall back to normal AcceptMostCommon logic.

## Preferences
- PreferNonEmpty (only with AcceptMostCommon)
  - Above threshold: if both a consensus-valid error and at least one non-empty meet threshold, pick the best non-empty (by count, then size).
  - Below threshold: with exactly one non-empty and at least one empty, pick the non-empty.
  - Also, when the leader by count above threshold is empty or consensus-error and any non-empty exists, pick the best non-empty.
- PreferLargerResponses
  - Below threshold (AcceptMostCommon): pick the largest non-empty.
  - Above threshold with multiple valid groups: pick the largest non-empty.
  - If best-by-count is a smaller non-empty and a larger non-empty exists: under AcceptMostCommon choose the largest; under ReturnError dispute.

## Threshold winners and ties
- If any valid group (non-empty, empty, consensus error) meets AgreementThreshold, it’s the baseline winner.
  - If winner is consensus-valid error, return that error.
  - If winner is non-empty or empty, return that result.
- Ties at/above threshold without preferences → dispute (ties among non-error groups; agreed error ties are handled by the threshold rule).

## Dispute cases (below threshold)
- Multiple valid groups exist but none meet the threshold → dispute.

## All infrastructure errors
- If validParticipants == 0 and an identical infrastructure error meets threshold → return that infra error.
- Otherwise under low participants, return LowParticipants error.

## Short-circuits (early exits)
- Consensus-valid error meets threshold → return error immediately unless PreferNonEmpty is enabled with AcceptMostCommon (we wait for a possible non-empty).
- Non-empty winner meets threshold and lead is unassailable given remaining participants → return result.
- Short-circuits are disabled when a preference could change the winner (e.g., PreferLarger, or PreferNonEmpty while empty leads).

## Error normalization
- Errors are compared by normalized JSON-RPC codes (e.g., execution reverted, invalid params, missing data).
- Only consensus-valid errors contribute to consensus; infrastructure errors are excluded from validParticipants and valid comparison groups.

## Configuration summary
- MaxParticipants, AgreementThreshold.
- DisputeBehavior: AcceptMostCommonValidResult | ReturnError | OnlyBlockHeadLeader | PreferBlockHeadLeader.
- LowParticipantsBehavior: AcceptMostCommonValidResult | ReturnError | OnlyBlockHeadLeader | PreferBlockHeadLeader.
- PreferNonEmpty, PreferLargerResponses, IgnoreFields.

## Notes
- Preferences never promote infrastructure errors.
- Leader selection uses the network’s EVM state (latest block) to identify the leader upstream.