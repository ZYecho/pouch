package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/alibaba/pouch/apis/filters"
	"github.com/alibaba/pouch/apis/metrics"
	"github.com/alibaba/pouch/apis/types"
	"github.com/alibaba/pouch/daemon/mgr"
	"github.com/alibaba/pouch/pkg/errtypes"
	"github.com/alibaba/pouch/pkg/httputils"
	util_metrics "github.com/alibaba/pouch/pkg/utils/metrics"

	"github.com/go-openapi/strfmt"
	"github.com/gorilla/mux"
	"github.com/opencontainers/go-digest"
	"github.com/sirupsen/logrus"
)

// pullImage will pull an image from a specified registry.
func (s *Server) pullImage(ctx context.Context, rw http.ResponseWriter, req *http.Request) error {
	image := req.FormValue("fromImage")
	tag := req.FormValue("tag")

	if image == "" {
		err := fmt.Errorf("fromImage cannot be empty")
		return httputils.NewHTTPError(err, http.StatusBadRequest)
	}

	if tag != "" {
		image = image + ":" + tag
	}

	label := util_metrics.ActionPullLabel

	// record the time spent during image pull procedure.
	defer func(start time.Time) {
		metrics.ImageActionsCounter.WithLabelValues(label).Inc()
		metrics.ImagePullSummary.WithLabelValues(image).Observe(util_metrics.SinceInMicroseconds(start))
		metrics.ImageActionsTimer.WithLabelValues(label).Observe(time.Since(start).Seconds())
	}(time.Now())

	// get registry auth from Request header
	authStr := req.Header.Get("X-Registry-Auth")
	authConfig := types.AuthConfig{}
	if authStr != "" {
		data := base64.NewDecoder(base64.URLEncoding, strings.NewReader(authStr))
		if err := json.NewDecoder(data).Decode(&authConfig); err != nil {
			return err
		}
	}
	// Error information has be sent to client, so no need call resp.Write
	if err := s.ImageMgr.PullImage(ctx, image, &authConfig, newWriteFlusher(rw)); err != nil {
		logrus.Errorf("failed to pull image %s: %v", image, err)
		if err == errtypes.ErrNotfound {
			return httputils.NewHTTPError(err, http.StatusNotFound)
		}
		return err
	}
	metrics.ImageSuccessActionsCounter.WithLabelValues(label).Inc()
	return nil
}

func (s *Server) getImage(ctx context.Context, rw http.ResponseWriter, req *http.Request) error {
	idOrRef := mux.Vars(req)["name"]

	imageInfo, err := s.ImageMgr.GetImage(ctx, idOrRef)
	if err != nil {
		logrus.Errorf("failed to get image: %v", err)
		return err
	}

	return EncodeResponse(rw, http.StatusOK, imageInfo)
}

func (s *Server) listImages(ctx context.Context, rw http.ResponseWriter, req *http.Request) error {
	filter, err := filters.FromParam(req.FormValue("filters"))
	if err != nil {
		return err
	}

	imageList, err := s.ImageMgr.ListImages(ctx, filter)
	if err != nil {
		logrus.Errorf("failed to list images: %v", err)
		return err
	}
	return EncodeResponse(rw, http.StatusOK, imageList)
}

func (s *Server) searchImages(ctx context.Context, rw http.ResponseWriter, req *http.Request) error {
	searchPattern := req.FormValue("term")
	registry := req.FormValue("registry")

	// get registry auth from Request header
	authStr := req.Header.Get("X-Registry-Auth")
	authConfig := types.AuthConfig{}
	if authStr != "" {
		data := base64.NewDecoder(base64.URLEncoding, strings.NewReader(authStr))
		if err := json.NewDecoder(data).Decode(&authConfig); err != nil {
			return err
		}
		if err := authConfig.Validate(strfmt.NewFormats()); err != nil {
			return err
		}
	}

	searchResultItem, err := s.ImageMgr.SearchImages(ctx, searchPattern, registry, &authConfig)
	if err != nil {
		logrus.Errorf("failed to search images from registry: %v", err)
		return err
	}
	return EncodeResponse(rw, http.StatusOK, searchResultItem)
}

// removeImage deletes an image by reference.
func (s *Server) removeImage(ctx context.Context, rw http.ResponseWriter, req *http.Request) error {
	name := mux.Vars(req)["name"]

	image, err := s.ImageMgr.GetImage(ctx, name)
	if err != nil {
		return err
	}

	refs, err := s.ImageMgr.ListReferences(ctx, digest.Digest(image.ID))
	if err != nil {
		return err
	}

	label := util_metrics.ActionDeleteLabel
	defer func(start time.Time) {
		metrics.ImageActionsCounter.WithLabelValues(label).Inc()
		metrics.ImageActionsTimer.WithLabelValues(label).Observe(time.Since(start).Seconds())
	}(time.Now())

	isForce := httputils.BoolValue(req, "force")

	isImageIDPrefix := func(imageID string, name string) bool {
		if strings.HasPrefix(imageID, name) || strings.HasPrefix(digest.Digest(imageID).Hex(), name) {
			return true
		}
		return false
	}

	// We should check the image whether used by container when there is only one primary reference
	// or the image is removed by image ID.
	if len(refs) == 1 || isImageIDPrefix(image.ID, name) {
		containers, err := s.ContainerMgr.List(ctx, &mgr.ContainerListOption{
			All: true,
			FilterFunc: func(c *mgr.Container) bool {
				return c.Image == image.ID
			}})
		if err != nil {
			return err
		}

		if !isForce && len(containers) > 0 {
			return fmt.Errorf("Unable to remove the image %q (must force) - container (%s, %s) is using this image", image.ID, containers[0].ID, containers[0].Name)
		}
	}

	if err := s.ImageMgr.RemoveImage(ctx, name, isForce); err != nil {
		return err
	}

	metrics.ImageSuccessActionsCounter.WithLabelValues(label).Inc()
	rw.WriteHeader(http.StatusNoContent)
	return nil
}

// postImageTag adds tag for the existing image.
func (s *Server) postImageTag(ctx context.Context, rw http.ResponseWriter, req *http.Request) error {
	name := mux.Vars(req)["name"]

	targetRef := req.FormValue("repo")
	if tag := req.FormValue("tag"); tag != "" {
		targetRef = fmt.Sprintf("%s:%s", targetRef, tag)
	}

	if err := s.ImageMgr.AddTag(ctx, name, targetRef); err != nil {
		return err
	}

	rw.WriteHeader(http.StatusCreated)
	return nil
}

// loadImage loads an image by http tar stream.
func (s *Server) loadImage(ctx context.Context, rw http.ResponseWriter, req *http.Request) error {
	imageName := req.FormValue("name")

	if err := s.ImageMgr.LoadImage(ctx, imageName, req.Body); err != nil {
		return err
	}

	rw.WriteHeader(http.StatusOK)
	return nil
}

// saveImage saves an image by http tar stream.
func (s *Server) saveImage(ctx context.Context, rw http.ResponseWriter, req *http.Request) error {
	imageName := req.FormValue("name")

	rw.Header().Set("Content-Type", "application/x-tar")

	r, err := s.ImageMgr.SaveImage(ctx, imageName)
	if err != nil {
		return err
	}
	defer r.Close()

	output := newWriteFlusher(rw)
	_, err = io.Copy(output, r)
	return err
}

// getImageHistory gets image history.
func (s *Server) getImageHistory(ctx context.Context, rw http.ResponseWriter, req *http.Request) error {
	imageName := mux.Vars(req)["name"]

	history, err := s.ImageMgr.ImageHistory(ctx, imageName)
	if err != nil {
		return err
	}

	return EncodeResponse(rw, http.StatusOK, history)
}

// pushImage will push an image to a specified registry.
func (s *Server) pushImage(ctx context.Context, rw http.ResponseWriter, req *http.Request) error {
	name := mux.Vars(req)["name"]
	tag := req.FormValue("tag")

	// get registry auth from Request header
	authStr := req.Header.Get("X-Registry-Auth")
	authConfig := types.AuthConfig{}
	if authStr != "" {
		data := base64.NewDecoder(base64.URLEncoding, strings.NewReader(authStr))
		if err := json.NewDecoder(data).Decode(&authConfig); err != nil {
			return err
		}
	}

	if err := s.ImageMgr.PushImage(ctx, name, tag, &authConfig, newWriteFlusher(rw)); err != nil {
		logrus.Errorf("failed to push image %s with tag %s: %v", name, tag, err)
		return err
	}

	return nil
}
