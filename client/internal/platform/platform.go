package platform

import "errors"

var ErrBusy = errors.New("file is locked by another process")
