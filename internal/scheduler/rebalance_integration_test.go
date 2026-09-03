package scheduler

// Integration coverage for runtime reallocation is intentionally kept small;
// the core arithmetic is covered in rebalance_test.go. The live scheduler path
// invokes rebalanceLive after every completion/admission event.
