// Copyright 2019 Matthew Crenshaw. All Rights Reserved.
// This file is available under the Apache license.

package logger

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
)

var (
	// DefaultLogger is used when a Logger is not provided
	DefaultLogger = &internalLogger{log: log.New(os.Stderr, "", log.LstdFlags)}
	// DiscardingLogger can be used to disable logging output
	DiscardingLogger = &internalLogger{log: log.New(ioutil.Discard, "", 0)}
)

// The Logger interface generalizes the Entry and Logger types
type Logger interface {
	Infof(format string, args ...interface{})
	Warningf(format string, args ...interface{})
	Errorf(format string, args ...interface{})
	Info(args ...interface{})
	Warning(args ...interface{})
	Error(args ...interface{})
}

type internalLogger struct {
	log *log.Logger
}

func (i *internalLogger) Infof(format string, args ...interface{}) {
	i.Info(fmt.Sprintf(format, args...))
}
func (i *internalLogger) Warningf(format string, args ...interface{}) {
	i.Warning(fmt.Sprintf(format, args...))
}
func (i *internalLogger) Errorf(format string, args ...interface{}) {
	i.Error(fmt.Sprintf(format, args...))
}
func (i *internalLogger) Info(args ...interface{}) {
	i.log.Println("I!", fmt.Sprint(args...))
}
func (i *internalLogger) Warning(args ...interface{}) {
	i.log.Println("W!", fmt.Sprint(args...))
}
func (i *internalLogger) Error(args ...interface{}) {
	i.log.Println("E!", fmt.Sprint(args...))
}
