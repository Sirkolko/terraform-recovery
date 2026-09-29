package recovery

import "errors"

// ErrLocked means another terraform-recovery session uses the project.
var ErrLocked = errors.New("another terraform-recovery session is already using this project")

// lockFileName is the lock file inside the recovery directory.
const lockFileName = "lock"
