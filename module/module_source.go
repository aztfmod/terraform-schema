package module

import (
	"fmt"
)

type ModuleSource struct {
	Source string
	Name   string
}

// MapKey returns a string that can be used to uniquely identify the receiver
// in a map[string]*moduleSource.
func (r *ModuleSource) MapKey() string {
	return fmt.Sprintf("module.%s", r.Name)
}
