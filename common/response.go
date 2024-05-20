package common

import (
	"io"
	"net/http"
	"sync"

	"github.com/rs/zerolog/log"
)

type ResponseWriter interface {
	TryLock() bool
	Unlock()
	AddHeader(string, string)
	Write([]byte) (int, error)
	AddBodyWriter(io.WriteCloser)
	Close() error
}

type HttpCompositeResponseWriter struct {
	sync.Mutex
	req          *NormalizedRequest
	headerWriter http.ResponseWriter
	bodyWriters  []io.Writer
}

func NewHttpCompositeResponseWriter(req *NormalizedRequest, hrw http.ResponseWriter) *HttpCompositeResponseWriter {
	return &HttpCompositeResponseWriter{
		req:          req,
		headerWriter: hrw,
		bodyWriters:  []io.Writer{hrw},
	}
}

func (w *HttpCompositeResponseWriter) AddBodyWriter(bwr io.WriteCloser) {
	w.Lock()
	w.bodyWriters = append(w.bodyWriters, bwr)
	w.Unlock()
}

func (w *HttpCompositeResponseWriter) AddHeader(key, value string) {
	if w.headerWriter != nil {
		w.headerWriter.Header().Add(key, value)
	}
}

func (w *HttpCompositeResponseWriter) Write(data []byte) (c int, err error) {
	if log.Trace().Enabled() {
		log.Trace().Msgf("writing response on bodyWriters: %v data: %s", len(w.bodyWriters), data)
	}

	for _, bw := range w.bodyWriters {
		c, err = bw.Write(data)
		if err != nil {
			log.Error().Err(err).Msg("failed to write response")
			return c, err
		}
	}

	return
}

func (w *HttpCompositeResponseWriter) Close() error {
	if w.bodyWriters != nil {
		for _, bw := range w.bodyWriters {
			closer, ok := bw.(io.Closer)
			if ok {
				if err := closer.Close(); err != nil {
					log.Error().Err(err).Msg("failed to close bodyWriter")
					return err
				}
			}
		}
	}

	if w.headerWriter != nil {
		if closer, ok := w.headerWriter.(io.Closer); ok {
			if err := closer.Close(); err != nil {
				log.Error().Err(err).Msg("failed to close headerWriter")
				return err
			}
		}
	}

	return nil
}
