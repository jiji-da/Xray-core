package dispatcher

import (
	"context"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"golang.org/x/time/rate"
)

type RateLimitedWriter struct {
	Limiter *rate.Limiter
	Writer  buf.Writer
}

func (w *RateLimitedWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	ctx := context.Background()
	for !mb.IsEmpty() {
		if err := w.Limiter.Wait(ctx); err != nil {
			return err
		}
		mb2, chunk := buf.SplitSize(mb, buf.Size)
		mb = mb2
		if err := w.Writer.WriteMultiBuffer(chunk); err != nil {
			return err
		}
	}

	return nil
}

func (w *RateLimitedWriter) Close() error {
	return common.Close(w.Writer)
}

func (w *RateLimitedWriter) Interrupt() {
	common.Interrupt(w.Writer)
}
