package main

import (
	"net/http"

	"github.com/flynn/flynn/Godeps/_workspace/src/golang.org/x/net/context"
	"github.com/flynn/flynn/pkg/httphelper"
	routerc "github.com/flynn/flynn/router/client"
	"github.com/flynn/flynn/router/types"
)

func (c *controllerAPI) CreateRoute(ctx context.Context, w http.ResponseWriter, req *http.Request) {
	var route router.Route
	if err := httphelper.DecodeJSON(req, &route); err != nil {
		respondWithError(w, err)
		return
	}

	route.ParentRef = routeParentRef(c.getApp(ctx).ID)
	if err := c.routerc.CreateRoute(&route); err != nil {
		respondWithError(w, err)
		return
	}
	httphelper.JSON(w, 200, &route)
}

func (c *controllerAPI) GetRoute(ctx context.Context, w http.ResponseWriter, req *http.Request) {
	route, err := c.getRoute(ctx)
	if err != nil {
		respondWithError(w, err)
		return
	}

	httphelper.JSON(w, 200, route)
}

func (c *controllerAPI) GetRouteList(ctx context.Context, w http.ResponseWriter, req *http.Request) {
	routes, err := c.routerc.ListRoutes(routeParentRef(c.getApp(ctx).ID))
	if err != nil {
		respondWithError(w, err)
		return
	}
	httphelper.JSON(w, 200, routes)
}

func (c *controllerAPI) DeleteRoute(ctx context.Context, w http.ResponseWriter, req *http.Request) {
	route, err := c.getRoute(ctx)
	if err != nil {
		respondWithError(w, err)
		return
	}

	err = c.routerc.DeleteRoute(route.ID)
	if err == routerc.ErrNotFound {
		err = ErrNotFound
	}
	if err != nil {
		respondWithError(w, err)
		return
	}
	w.WriteHeader(200)
}
