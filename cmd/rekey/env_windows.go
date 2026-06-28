//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
)

func hostEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			out = append(out, kv)
			continue
		}
		switch strings.ToUpper(key) {
		case "PATH":
			value = windowsPathList(value)
		case "GOBIN", "HOME", "PWD", "OLDPWD":
			value = windowsPath(value)
		case "GOPATH":
			value = windowsPathList(value)
		}
		out = append(out, key+"="+value)
	}
	return out
}

func hostExecutable(name string) string {
	if path := lookPathInEnv(name, hostEnv()); path != "" {
		return path
	}
	return name
}

func hostPathEnv(key string) string {
	return windowsPath(os.Getenv(key))
}

func hostPathListEnv(key string) string {
	return windowsPathList(os.Getenv(key))
}

func windowsPathList(value string) string {
	if value == "" {
		return ""
	}
	var parts []string
	if strings.Contains(value, ";") {
		parts = strings.Split(value, ";")
	} else if strings.Contains(strings.ToLower(value), "/cygdrive/") {
		parts = strings.Split(value, ":")
	} else {
		return windowsPath(value)
	}
	for i, part := range parts {
		parts[i] = windowsPath(part)
	}
	return strings.Join(parts, ";")
}

func windowsPath(value string) string {
	lower := strings.ToLower(value)
	const prefix = "/cygdrive/"
	if !strings.HasPrefix(lower, prefix) || len(value) < len(prefix)+1 {
		return value
	}
	drive := value[len(prefix)]
	if !((drive >= 'a' && drive <= 'z') || (drive >= 'A' && drive <= 'Z')) {
		return value
	}
	rest := value[len(prefix)+1:]
	rest = strings.TrimPrefix(rest, "/")
	rest = strings.ReplaceAll(rest, "/", `\`)
	drive = byte(strings.ToUpper(string(drive))[0])
	if rest == "" {
		return string([]byte{drive}) + `:\`
	}
	return string([]byte{drive}) + `:\` + rest
}

func lookPathInEnv(name string, env []string) string {
	pathValue := envValue(env, "PATH")
	if pathValue == "" {
		return ""
	}
	extensions := []string{""}
	if filepath.Ext(name) == "" {
		pathext := envValue(env, "PATHEXT")
		if pathext == "" {
			pathext = ".COM;.EXE;.BAT;.CMD"
		}
		for _, ext := range strings.Split(pathext, ";") {
			if ext != "" {
				extensions = append(extensions, ext)
			}
		}
	}
	for _, dir := range strings.Split(pathValue, ";") {
		if dir == "" {
			dir = "."
		}
		for _, ext := range extensions {
			path := filepath.Join(dir, name+ext)
			if executableFile(path) {
				return path
			}
		}
	}
	return ""
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

func executableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
