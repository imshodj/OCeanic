package agent

import "runtime"

func runtimeArch() string { return runtime.GOARCH }
