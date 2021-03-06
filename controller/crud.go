package main

import (
	"net/http"
	"reflect"

	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/julienschmidt/httprouter"
	"github.com/flynn/flynn/Godeps/_workspace/src/golang.org/x/net/context"
	"github.com/flynn/flynn/pkg/httphelper"
)

type Repository interface {
	Add(thing interface{}) error
	Get(id string) (interface{}, error)
	List() (interface{}, error)
}

type Remover interface {
	Remove(string) error
}

type Updater interface {
	Update(string, map[string]interface{}) (interface{}, error)
}

func crud(r *httprouter.Router, resource string, example interface{}, repo Repository) {
	resourceType := reflect.TypeOf(example)
	prefix := "/" + resource

	r.POST(prefix, httphelper.WrapHandler(func(ctx context.Context, rw http.ResponseWriter, req *http.Request) {
		thing := reflect.New(resourceType).Interface()
		if err := httphelper.DecodeJSON(req, thing); err != nil {
			respondWithError(rw, err)
			return
		}

		err := repo.Add(thing)
		if err != nil {
			respondWithError(rw, err)
			return
		}
		httphelper.JSON(rw, 200, thing)
	}))

	lookup := func(ctx context.Context) (interface{}, error) {
		return repo.Get(httphelper.ParamsFromContext(ctx).ByName(resource + "_id"))
	}

	singletonPath := prefix + "/:" + resource + "_id"
	r.GET(singletonPath, httphelper.WrapHandler(func(ctx context.Context, rw http.ResponseWriter, _ *http.Request) {
		thing, err := lookup(ctx)
		if err != nil {
			respondWithError(rw, err)
			return
		}
		httphelper.JSON(rw, 200, thing)
	}))

	r.GET(prefix, httphelper.WrapHandler(func(ctx context.Context, rw http.ResponseWriter, _ *http.Request) {
		list, err := repo.List()
		if err != nil {
			respondWithError(rw, err)
			return
		}
		httphelper.JSON(rw, 200, list)
	}))

	if remover, ok := repo.(Remover); ok {
		r.DELETE(singletonPath, httphelper.WrapHandler(func(ctx context.Context, rw http.ResponseWriter, _ *http.Request) {
			_, err := lookup(ctx)
			if err != nil {
				respondWithError(rw, err)
				return
			}
			params := httphelper.ParamsFromContext(ctx)
			if err = remover.Remove(params.ByName(resource + "_id")); err != nil {
				respondWithError(rw, err)
				return
			}
			rw.WriteHeader(200)
		}))
	}

	if updater, ok := repo.(Updater); ok {
		r.POST(singletonPath, httphelper.WrapHandler(func(ctx context.Context, rw http.ResponseWriter, req *http.Request) {
			params := httphelper.ParamsFromContext(ctx)

			var data map[string]interface{}
			if err := httphelper.DecodeJSON(req, &data); err != nil {
				respondWithError(rw, err)
				return
			}
			app, err := updater.Update(params.ByName(resource+"_id"), data)
			if err != nil {
				respondWithError(rw, err)
				return
			}
			httphelper.JSON(rw, 200, app)
		}))
	}
}
