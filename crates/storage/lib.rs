//! Rust port of ODM's `internal/storage` package: on-disk side of a download
//! task — pre-allocation + concurrent positioned writes (`File::write_at`)
//! and the `.odm` control file used for resume.

pub mod file;
pub mod resume;

pub use file::File;
pub use resume::*;
