package common

import (
	"net/http"
	"sync"

	"github.com/rs/zerolog/log"
)

type ResponseWriter interface {
	TryLock() bool
	Unlock()
	AddHeader(string, string)
	Write([]byte) (int, error)
	SetDAL(DAL)
}

type HttpCompositeResponseWriter struct {
	sync.Mutex
	req *NormalizedRequest
	hrw http.ResponseWriter
	dal DAL
}

func NewHttpCompositeResponseWriter(req *NormalizedRequest, hrw http.ResponseWriter) *HttpCompositeResponseWriter {
	return &HttpCompositeResponseWriter{
		req: req,
		hrw: hrw,
	}
}

func (w *HttpCompositeResponseWriter) SetDAL(dal DAL) {
	w.dal = dal
}

func (w *HttpCompositeResponseWriter) AddHeader(key, value string) {
	if w.hrw != nil {
		w.hrw.Header().Add(key, value)
	}
}

func (w *HttpCompositeResponseWriter) Write(data []byte) (c int, err error) {
	if log.Trace().Enabled() {
		log.Trace().Msgf("writing response on http: %v dal: %v data: %s", &w.hrw, &w.dal, data)
	}
	if w.hrw != nil {
		c, err = w.hrw.Write(data)
		if err != nil {
			return
		}
	}

	if w.dal != nil {
		c, err = w.dal.Set(w.req, data)
		if err != nil {
			return
		}
	}

	return
}
