#ifndef ODM_CB_SHIM_H
#define ODM_CB_SHIM_H

/* Rust-side progress callback: receives one NUL-terminated JSON event. */
typedef void (*odm_progress_cb)(const char *event_json);

void odm_shim_set_progress(odm_progress_cb cb);
void odm_shim_progress(const char *event_json);

#endif /* ODM_CB_SHIM_H */
