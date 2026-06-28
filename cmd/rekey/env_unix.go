//go:build !windows

package main

import "os"

func hostEnv() []string {
	return os.Environ()
}

func hostExecutable(name string) string {
	return name
}

func hostPathEnv(key string) string {
	return os.Getenv(key)
}

func hostPathListEnv(key string) string {
	return os.Getenv(key)
}
