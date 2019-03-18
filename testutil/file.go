package testutil

import (
	"github.com/sgtsquiggs/tail/logger"
	"os"
	"testing"
)

func WriteString(tb testing.TB, f *os.File, str string) int {
	tb.Helper()
	n, err := f.WriteString(str)
	FatalIfErr(tb, err)
	logger.DefaultLogger.Infof("Wrote %d bytes", n)
	return n
}
