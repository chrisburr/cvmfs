/* Bridges libcvmfs log output to Go.  Kept in a separate C file because a Go
 * file containing //export directives must not define C functions in its
 * preamble. */

#include "libcvmfs.h"

extern void goCvmfsLog(char *msg);

static void cvmfs_log_relay(const char *msg) { goCvmfsLog((char *)msg); }

void install_cvmfs_log_relay(void) { cvmfs_set_log_fn(cvmfs_log_relay); }
