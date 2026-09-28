package main

import (
	"regexp"
	"strings"
	"sync/atomic"
)

const defaultCloudPluginID = "codex-turn-state"

var cloudPluginID atomic.Value
var cloudPluginIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func setCloudPluginID(resourceBase string) {
	id := strings.TrimPrefix(strings.TrimRight(resourceBase, "/"), "/v0/resource/plugins/")
	if !strings.HasPrefix(resourceBase, "/v0/resource/plugins/") || !cloudPluginIDPattern.MatchString(id) {
		id = defaultCloudPluginID
	}
	cloudPluginID.Store(id)
}

func currentCloudPluginID() string {
	if id, ok := cloudPluginID.Load().(string); ok {
		return id
	}
	return defaultCloudPluginID
}

func cloudRegisteredPath(path string) string {
	return strings.Replace(path, "/"+defaultCloudPluginID+"/", "/"+currentCloudPluginID()+"/", 1)
}

func cloudCanonicalPath(path string) string {
	id := currentCloudPluginID()
	if id == defaultCloudPluginID {
		return path
	}
	for _, prefix := range []string{"/v0/management/", "/v0/resource/plugins/", "/"} {
		if strings.HasPrefix(path, prefix+id+"/") {
			return prefix + defaultCloudPluginID + "/" + strings.TrimPrefix(path, prefix+id+"/")
		}
	}
	return path
}
