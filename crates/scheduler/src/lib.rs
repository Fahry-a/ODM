//! Rust port of ODM's `internal/scheduler` package: the Connection Balancer
//! (pure function in `balancer`), the live `Scheduler` (`scheduler`, port of
//! queue.go) and the RPC-facing `Daemon` (`daemon`).

pub mod balancer;
pub mod daemon;
pub mod rebalance;
pub mod scheduler;

pub use balancer::{compute, Allocation, FileInput, Plan, DEFAULT_MAX_CONNECTIONS};
pub use daemon::{new_daemon, Daemon, OnDeadFn};
pub use scheduler::{OnCompleteFn, ProgressCB, ScheduledTask, Scheduler, TaskMaker};
