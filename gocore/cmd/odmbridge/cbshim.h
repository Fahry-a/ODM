#ifndef ODM_CB_SHIM_H
#define ODM_CB_SHIM_H

/* Rust-side progress callback: receives one NUL-terminated JSON event. */
typedef void (*odm_progress_cb)(const char *event_json);
/* Rust-side log callback: level (info/warn/error) and the message. */
typedef void (*odm_log_cb)(const char *level, const char *message);

void odm_shim_set_progress(odm_progress_cb cb);
void odm_shim_progress(const char *event_json);
void odm_shim_set_log(odm_log_cb cb);
void odm_shim_log(const char *level, const char *message);

#endif /* ODM_CB_SHIM_H */
