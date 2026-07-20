package autostart

import "context"

type ServiceApp interface {
	Start() error
	Stop(ctx context.Context)
	Errors() <-chan error
	Logger() Logger
}

type Logger interface {
	Info(msg string)
	Error(msg string, err error)
}
