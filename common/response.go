package common

import (
	"bytes"
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
	headerWriter http.ResponseWriter
	bodyWriters  []io.Writer
}

func NewHttpCompositeResponseWriter(hrw http.ResponseWriter) *HttpCompositeResponseWriter {
	return &HttpCompositeResponseWriter{
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

type MemoryResponseWriter struct {
	http.ResponseWriter
	Body *bytes.Buffer
}

func NewMemoryResponseWriter() *MemoryResponseWriter {
	return &MemoryResponseWriter{
		Body: new(bytes.Buffer),
	}
}

func (crw *MemoryResponseWriter) Write(b []byte) (int, error) {
	return crw.Body.Write(b)
}

// func (crw *MemoryResponseWriter) WriteHeader(statusCode int) {
// 	// crw.StatusCode = statusCode
// 	// crw.ResponseWriter.WriteHeader(statusCode)
// }

func (crw *MemoryResponseWriter) Header() http.Header {
	return http.Header{}
}

func (crw *MemoryResponseWriter) Read(p []byte) (int, error) {
	return crw.Body.Read(p)
}
