//! Rust port of ODM's `internal/download` package: task lifecycle, chunk
//! queue (work-stealing), Manager, exit codes.

pub mod chunkqueue;
pub mod engine;
pub mod manager;
pub mod resume;
pub mod smart;
pub mod task;
pub mod task_checksum;
pub mod task_filename;
pub mod task_io;
pub mod task_pause;
pub mod task_persist;
pub mod task_start;

pub use chunkqueue::{Chunk, ChunkQueue, StaticQueue, WorkQueue};
pub use engine::{aria_split as AriaSplit, Engine};
pub use manager::{exit_code, exit_code_from, ExecOptions, Manager};
pub use smart::{choose_profile as ChooseProfile, ServerCapabilities};
pub use task::{Ctx, LogFn, ProgressView, Sink, Task, TaskOptions, TaskState, TaskID};
