package main

func normalizeExecArgs(args []string) []string {
	if len(args) != 0 && args[0] == "--" {
		return args[1:]
	}
	return args
}

func breadcrumbexecUsage() string {
	return "usage: breadcrumbexec [--] program [args...]"
}
