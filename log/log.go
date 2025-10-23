package log

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

var LogLevel int = 0
var cached bool = false

const (
	reset  = "\033[0m"
	gray   = "\033[90m"
	blue   = "\033[34m"
	yellow = "\033[33m"
	red    = "\033[31m"
	green  = "\033[32m"
)

func getLogLevel() int {
	if cached {
		return LogLevel
	} else {
		level, done := os.LookupEnv("LOG_LEVEL")

		if done {
			val, err := strconv.Atoi(level)

			if err == nil {
				LogLevel = val
			} else {
				LogLevel = 1
			}
		} else {
			LogLevel = 0
		}

		cached = true
		return LogLevel
	}
}

func writeConsole(color string, tag string, format any, a ...any) {
	utc := time.Now().UTC()
	timeStamp := fmt.Sprintf("%s UTC", utc.Format(time.RFC3339))

	level := fmt.Sprintf("%s| %s |", color, tag)

	var message string
	switch v := format.(type) {
	case string:
		if len(a) > 0 {
			message = fmt.Sprintf(v, a...)
		} else {
			message = v
		}
	default:
		message = fmt.Sprint(format)
	}

	fmt.Println(timeStamp, level, message, reset)
}

func Debug(format any, a ...any) {
	if getLogLevel() <= 0 {
		writeConsole(gray, "DEBUG", format, a...)
	}
}

func Info(format any, a ...any) {
	if getLogLevel() <= 1 {
		writeConsole(blue, "INFO", format, a...)
	}
}

func Warn(format any, a ...any) {
	if getLogLevel() <= 2 {
		writeConsole(yellow, "WARN", format, a...)
	}
}

func Error(format any, a ...any) {
	if getLogLevel() <= 3 {
		writeConsole(red, "ERROR", format, a...)
	}
}

func Done(format any, a ...any) {
	if getLogLevel() <= 4 {
		writeConsole(green, "DONE", format, a...)
	}
}

func Print(format any, a ...any) {
	if getLogLevel() <= 5 {
		writeConsole(reset, " LOG ", format, a...)
	}
}
