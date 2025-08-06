# Internal doc: Consensus policy

## Overview

The consensus policy is a sophisticated fault-tolerant mechanism that collects responses from multiple upstream RPC providers and determines the most reliable result through advanced agreement algorithms. It operates in three distinct phases and handles complex scenarios including network failures, execution errors, and edge cases.

- Response Gathering
  - Short-circuit Analysis
    - If prefer non-empty or prefer larger flags are enabled and there are more participants left, continue...
    - If there's a clear winner and it meets the threshold let's exit early
- Consensus Evaluation
  - Winner Selection
    - Priority Order: Largest, Non-empty, Empty, Consensused Error, Generic Error
    - Agreement Threshold
      - Consider it "low participants" if none of the groups reaches agreement threshold
      - The winner must meet the agreement threshold in normal flow
    - Prefer Non-empty, Prefer Largest
      - For groups above the threshold apply the priority ordering despite their count
        - If none of preference flags exist then highest count should win
        - In case of a tie between highest count break it via priority ordering
          - If still no clear winner then return dispute
      - Dispute if winner is empty but any non-empty results exists (even if they don't meet threshold)
      - Dispute if winner is smaller but other non-empty groups exist that are larger (even if they don't meet threshold)
- Dispute / Low Participant Behavior
  - If ReturnError is selected, return the error as is (either dispute or lowParticipants)
  - If AcceptMostCommonValidResult is selected
    - Re-select the winner but this time ignore the agreement threshold, simply run the whole algorithm above
  - If OnlyBlockHeadLeader is selected
    - Re-select the winner that incldues the block header leader (as long as response is empty or non-empty, ignore errors), otherwise return the dispute/lowParticipant error as is
  - If PreferBlockHeadLeader is selected
    - In case a non-error group  one with block head leader, otherwise fallback to AcceptMostCommonValidResult logic
- Misbehavior Tracking
  - In case of a winner that has met the threshold and count is majority, punish other upstreams