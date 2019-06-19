package mgr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/alibaba/pouch/apis/filters"
	"github.com/alibaba/pouch/apis/types"
	"github.com/alibaba/pouch/ctrd"
	"github.com/alibaba/pouch/daemon/config"
	"github.com/alibaba/pouch/daemon/events"
	"github.com/alibaba/pouch/hookplugins"
	"github.com/alibaba/pouch/pkg/errtypes"
	"github.com/alibaba/pouch/pkg/jsonstream"
	"github.com/alibaba/pouch/pkg/reference"
	"github.com/alibaba/pouch/pkg/utils"
	searchtypes "github.com/alibaba/pouch/registry/types"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/content"
	ctrdmetaimages "github.com/containerd/containerd/images"
	"github.com/containerd/containerd/platforms"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	pkgerrors "github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// The daemon will load all the images from containerd into memory. At
// the beginning, we assume that it can load it in 10 secs. But if the
// system has busy IO, it will take long time to load it, especially the
// more-layers and huge-size images. So update it from 10 secs to 10 mins.
var deadlineLoadImagesAtBootup = time.Minute * 10

// the filter tags set allowed when pouch images -f
var acceptedImageFilterTags = map[string]bool{
	"before":    true,
	"since":     true,
	"reference": true,
}

// ImageMgr as an interface defines all operations against images.
type ImageMgr interface {
	// LookupImageReferences find possible image reference list.
	LookupImageReferences(ref string) []string

	// PullImage pulls images from specified registry.
	PullImage(ctx context.Context, ref string, authConfig *types.AuthConfig, out io.Writer) error

	// PushImage pushes image to specified registry.
	PushImage(ctx context.Context, name, tag string, authConfig *types.AuthConfig, out io.Writer) error

	// GetImage returns imageInfo by reference or id.
	GetImage(ctx context.Context, idOrRef string) (*types.ImageInfo, error)

	// ListImages lists images stored by containerd.
	ListImages(ctx context.Context, filter filters.Args) ([]types.ImageInfo, error)

	// Search Images from specified registry.
	SearchImages(ctx context.Context, name, registry string, authConfig *types.AuthConfig) ([]types.SearchResultItem, error)

	// RemoveImage deletes an image by reference.
	RemoveImage(ctx context.Context, idOrRef string, force bool) error

	// AddTag creates target ref for source image.
	AddTag(ctx context.Context, sourceImage string, targetRef string) error

	// CheckReference returns imageID, actual reference and primary reference.
	CheckReference(ctx context.Context, idOrRef string) (digest.Digest, reference.Named, reference.Named, error)

	// ListReferences returns all references
	ListReferences(ctx context.Context, imageID digest.Digest) ([]reference.Named, error)

	// LoadImage creates a set of images by tarstream.
	LoadImage(ctx context.Context, imageName string, tarstream io.ReadCloser) error

	// SaveImage saves image to tarstream.
	SaveImage(ctx context.Context, idOrRef string) (io.ReadCloser, error)

	// ImageHistory returns image history by reference.
	ImageHistory(ctx context.Context, idOrRef string) ([]types.HistoryResultItem, error)

	// StoreImageReference update image reference.
	StoreImageReference(ctx context.Context, img containerd.Image) error

	// GetOCIImageConfig returns the image config of OCI
	GetOCIImageConfig(ctx context.Context, image string) (ocispec.ImageConfig, error)
}

// ImageManager is an implementation of interface ImageMgr.
type ImageManager struct {
	// DefaultRegistry is the default registry of daemon.
	// When users do not specify image repo in image name,
	// daemon will automatically pull images with DefaultRegistry and DefaultNamespace.
	DefaultRegistry string

	// DefaultNamespace is the default namespace used in DefaultRegistry.
	DefaultNamespace string

	// RegistryMirrors is a list of registry URLs that act as a mirror for the default registry.
	RegistryMirrors []string

	// client is a interface to the containerd client.
	// It is used to interact with containerd.
	client ctrd.APIClient

	// localStore is local cache of image reference information.
	localStore *imageStore

	// eventsService is used to publish events generated by pouchd
	eventsService *events.Events

	// imagePlugin is a plugin called before image operations
	imagePlugin hookplugins.ImagePlugin
}

// NewImageManager initializes a brand new image manager.
func NewImageManager(cfg *config.Config, client ctrd.APIClient, eventsService *events.Events, imagePlugin hookplugins.ImagePlugin) (*ImageManager, error) {
	store, err := newImageStore()
	if err != nil {
		return nil, err
	}

	mgr := &ImageManager{
		DefaultRegistry:  cfg.DefaultRegistry,
		DefaultNamespace: cfg.DefaultRegistryNS,
		RegistryMirrors:  cfg.RegistryMirrors,

		client:        client,
		localStore:    store,
		eventsService: eventsService,
		imagePlugin:   imagePlugin,
	}

	if err := mgr.updateLocalStore(); err != nil {
		return nil, err
	}
	return mgr, nil
}

// LookupImageReferences find possible image reference list.
func (mgr *ImageManager) LookupImageReferences(ref string) []string {
	var (
		registry  string
		remainder string
	)

	// extract the domain field
	idx := strings.IndexRune(ref, '/')
	if idx != -1 && strings.ContainsAny(ref[:idx], ".:") {
		registry, remainder = ref[:idx], ref[idx+1:]
	} else {
		remainder = ref
	}

	// create a list of reference name in order of RegistryMirrors, DefaultRegistry
	// for partial reference like 'ns/ubuntu', 'ubuntu'
	var fullRefs []string

	// if the domain field is empty, concat the ref with registry mirror urls.
	if registry == "" {
		for _, reg := range mgr.RegistryMirrors {
			fullRefs = append(fullRefs, path.Join(reg, ref))
		}
		registry = mgr.DefaultRegistry
	}

	// attach the default namespace if the registry match the default registry.
	if registry == mgr.DefaultRegistry && !strings.ContainsAny(remainder, "/") {
		remainder = mgr.DefaultNamespace + "/" + remainder
	}

	fullRefs = append(fullRefs, registry+"/"+remainder)

	return fullRefs
}

// PullImage pulls images from specified registry.
func (mgr *ImageManager) PullImage(ctx context.Context, ref string, authConfig *types.AuthConfig, out io.Writer) error {
	namedRef, err := reference.Parse(ref)
	if err != nil {
		return err
	}

	pctx, cancel := context.WithCancel(ctx)
	stream := jsonstream.New(out, nil)

	closeStream := func() {
		// close and wait stream
		stream.Close()
		stream.Wait()
		cancel()
	}

	writeStream := func(err error) {
		// Send Error information to client through stream
		message := jsonstream.JSONMessage{
			Error: &jsonstream.JSONError{
				Code:    http.StatusInternalServerError,
				Message: err.Error(),
			},
			ErrorMessage: err.Error(),
		}
		stream.WriteObject(message)
		closeStream()
	}

	fullRefs := mgr.LookupImageReferences(ref)
	namedRef = reference.TrimTagForDigest(reference.WithDefaultTagIfMissing(namedRef))

	img, err := mgr.client.PullImage(pctx, namedRef.String(), fullRefs, authConfig, stream)
	if err != nil {
		writeStream(err)
		return err
	}

	closeStream()

	// NOTE: pull image with different snapshotter, refer #2574
	// clean snapshotter key if has been set, not allow
	// user set except through image plugin
	ctx = ctrd.CleanSnapshotter(ctx)
	// call plugin before pull image
	if mgr.imagePlugin != nil {
		if err = mgr.imagePlugin.PostPull(ctx, ctrd.CurrentSnapshotterName(ctx), img); err != nil {
			logrus.Errorf("failed to execute post pull plugin: %s", err)
			return err
		}
	}

	mgr.LogImageEvent(ctx, img.Name(), namedRef.String(), "pull")

	return mgr.StoreImageReference(ctx, img)
}

// PushImage pushes image to specified registry.
func (mgr *ImageManager) PushImage(ctx context.Context, name, tag string, authConfig *types.AuthConfig, out io.Writer) error {
	ref, err := reference.Parse(name)
	if err != nil {
		return err
	}

	if tag == "" {
		ref = reference.WithDefaultTagIfMissing(ref)
	} else {
		ref = reference.WithTag(ref, tag)
	}

	return mgr.client.PushImage(ctx, ref.String(), authConfig, out)
}

// GetImage returns imageInfo by reference.
func (mgr *ImageManager) GetImage(ctx context.Context, idOrRef string) (*types.ImageInfo, error) {
	id, _, _, err := mgr.CheckReference(ctx, idOrRef)
	if err != nil {
		return nil, err
	}

	imgInfo, err := mgr.containerdImageToImageInfo(ctx, id)
	if err != nil {
		return nil, err
	}
	return &imgInfo, nil
}

// ListImages lists images stored by containerd.
func (mgr *ImageManager) ListImages(ctx context.Context, filter filters.Args) ([]types.ImageInfo, error) {
	if err := filter.Validate(acceptedImageFilterTags); err != nil {
		return nil, err
	}

	beforeImages := filter.Get("before")
	sinceImages := filter.Get("since")
	referenceFilter := filter.Get("reference")

	// refuse undefined behavior
	if len(beforeImages) > 1 {
		return nil, pkgerrors.Wrapf(errtypes.ErrInvalidParam, "can't use before filter more than one")
	}
	// refuse undefined behavior
	if len(sinceImages) > 1 {
		return nil, pkgerrors.Wrapf(errtypes.ErrInvalidParam, "can't use since filter more than one")
	}

	ctrdImageInfos := mgr.localStore.ListCtrdImageInfo()
	imgInfos := make([]types.ImageInfo, 0, len(ctrdImageInfos))

	var (
		beforeFilter, sinceFilter *types.ImageInfo
		beforeTime, sinceTime     time.Time
		err                       error
	)

	if len(beforeImages) > 0 {
		beforeFilter, err = mgr.GetImage(ctx, beforeImages[0])
		if err != nil {
			return nil, err
		}
		beforeTime, err = time.Parse(utils.TimeLayout, beforeFilter.CreatedAt)
		if err != nil {
			return nil, err
		}
	}

	if len(sinceImages) > 0 {
		sinceFilter, err = mgr.GetImage(ctx, sinceImages[0])
		if err != nil {
			return nil, err
		}
		sinceTime, err = time.Parse(utils.TimeLayout, sinceFilter.CreatedAt)
		if err != nil {
			return nil, err
		}
	}

	for _, img := range ctrdImageInfos {
		if beforeFilter != nil {
			if img.OCISpec.Created.Equal(beforeTime) || img.OCISpec.Created.After(beforeTime) {
				continue
			}
		}
		if sinceFilter != nil {
			if img.OCISpec.Created.Equal(sinceTime) || img.OCISpec.Created.Before(sinceTime) {
				continue
			}
		}

		imgInfo, err := mgr.containerdImageToImageInfo(ctx, img.ID)
		if err != nil {
			logrus.Warnf("failed to convert containerd image(%v) to ImageInfo during list images: %v", img.ID, err)
			continue
		}

		if len(referenceFilter) == 0 {
			imgInfos = append(imgInfos, imgInfo)
			continue
		}

		// do reference filter
		imgInfo.RepoDigests, err = filterReference(referenceFilter, imgInfo.RepoDigests)
		if err != nil {
			return []types.ImageInfo{}, err
		}

		imgInfo.RepoTags, err = filterReference(referenceFilter, imgInfo.RepoTags)
		if err != nil {
			return []types.ImageInfo{}, err
		}

		if len(imgInfo.RepoTags) > 0 || len(imgInfo.RepoDigests) > 0 {
			imgInfos = append(imgInfos, imgInfo)
		}

	}
	return imgInfos, nil
}

// SearchImages searches imaged from specified registry.
func (mgr *ImageManager) SearchImages(ctx context.Context, name, registry string, auth *types.AuthConfig) ([]types.SearchResultItem, error) {
	// Directly send API calls towards specified registry
	if len(registry) == 0 {
		registry = "https://" + mgr.DefaultRegistry + "/v1/"
	}

	u := registry + "search?q=" + url.QueryEscape(name)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}

	if auth != nil && auth.Username != "" && auth.Password != "" {
		req.SetBasicAuth(auth.Username, auth.Password)
	}

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		return nil, fmt.Errorf("unexepected status code %d", res.StatusCode)
	}

	rawData, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	var searchResultResp searchtypes.SearchResultResp
	var result []types.SearchResultItem
	err = json.Unmarshal(rawData, &searchResultResp)
	if err != nil {
		return nil, err
	}

	// TODO: sort results by count num
	for _, searchResultItem := range searchResultResp.Results {
		result = append(result, *searchResultItem)
	}
	return result, err
}

// RemoveImage deletes a reference.
//
// NOTE: if the reference is short ID or ID, should remove all the references.
func (mgr *ImageManager) RemoveImage(ctx context.Context, idOrRef string, force bool) error {
	id, namedRef, primaryRef, err := mgr.CheckReference(ctx, idOrRef)
	if err != nil {
		return err
	}

	// since there is no rollback functionality, no guarantee that the
	// containerd.RemoveImage must success. so if the localStore has been
	// remove all the primary references, we should clear the CtrdImageInfo
	// cache.
	defer func() {
		if len(mgr.localStore.GetPrimaryReferences(id)) == 0 {
			mgr.localStore.ClearCtrdImageInfo(id)
		}
	}()

	// should remove all the references if the reference is ID (Named Only)
	// or Digest ID (Tagged Named)
	if reference.IsNamedOnly(namedRef) ||
		strings.HasPrefix(id.String(), namedRef.String()) {

		// NOTE: the user maybe use the following references to pull one image
		//
		//	busybox:1.25
		//	busybox@sha256:29f5d56d12684887bdfa50dcd29fc31eea4aaf4ad3bec43daf19026a7ce69912
		//
		// There are referencing to the same image. They have the same
		// locator though there are two primary references. For this
		// case, we should remove two primary references without force
		// option.
		//
		// However, if there is alias like localhost:5000/busybox:latest
		// as searchable reference, we cannot remove the image because
		// the searchable reference has different locator without force.
		// It's different reference from locator aspect.
		if !force && !uniqueLocatorReference(mgr.localStore.GetReferences(id)) {
			return fmt.Errorf("Unable to remove the image %q (must force) - image has serveral references", idOrRef)
		}

		for _, ref := range mgr.localStore.GetPrimaryReferences(id) {
			if err := mgr.client.RemoveImage(ctx, ref.String()); err != nil {
				return err
			}

			if err := mgr.localStore.RemoveReference(id, ref); err != nil {
				return err
			}
		}
		return nil
	}

	namedRef = reference.TrimTagForDigest(namedRef)
	// remove the image if the nameRef is primary reference
	if primaryRef.String() == namedRef.String() {
		if err := mgr.localStore.RemoveReference(id, primaryRef); err != nil {
			return err
		}

		return mgr.client.RemoveImage(ctx, primaryRef.String())
	}

	// untag event
	mgr.LogImageEvent(ctx, namedRef.String(), namedRef.String(), "untag")
	return mgr.localStore.RemoveReference(id, namedRef)
}

// AddTag adds the tag reference to the source image.
//
// NOTE(fuwei): AddTag hacks the containerd metadata boltdb, which we add the
// reference into the containerd metadata boltdb with the existing image content.
// It means that the "tag" is primary reference in the pouchd.
//
// For example,
//	pouch tag A B
//	pouch rmi A
//
// The B is still there.
func (mgr *ImageManager) AddTag(ctx context.Context, sourceImage string, targetTag string) error {
	targetTag = addDefaultRegistryIfMissing(targetTag, mgr.DefaultRegistry, mgr.DefaultNamespace)

	tagRef, err := parseTagReference(targetTag)
	if err != nil {
		return err
	}

	if err := mgr.validateTagReference(tagRef); err != nil {
		return err
	}

	ctrdImg, err := mgr.fetchContainerdImage(ctx, sourceImage)
	if err != nil {
		return err
	}

	// add the reference into memory
	cfg, err := ctrdImg.Config(ctx)
	if err != nil {
		return err
	}
	if err := mgr.addReferenceIntoStore(cfg.Digest, tagRef, ctrdImg.Target().Digest); err != nil {
		return err
	}

	// add the reference into containerd meta db
	_, err = mgr.client.CreateImageReference(ctx, ctrdmetaimages.Image{
		Name:   tagRef.String(),
		Target: ctrdImg.Target(),
	})
	return err
}

// ImageHistory returns image history by reference.
func (mgr *ImageManager) ImageHistory(ctx context.Context, idOrRef string) ([]types.HistoryResultItem, error) {
	img, err := mgr.fetchContainerdImage(ctx, idOrRef)
	if err != nil {
		return nil, err
	}

	desc, err := img.Config(ctx)
	if err != nil {
		return nil, err
	}

	ociImage, err := containerdImageToOciImage(ctx, img)
	if err != nil {
		return nil, err
	}

	cs := img.ContentStore()
	manifest, err := mgr.getManifest(ctx, cs, img, platforms.Default())
	if err != nil {
		return nil, err
	}

	ociImageHistory := ociImage.History
	lenOciImageHistory := len(ociImageHistory)
	history := make([]types.HistoryResultItem, lenOciImageHistory)
	// Note: ociImage History layers info and manifest layers info are all in order from bottom-most to top-most, but the
	// user-interactive history is in order from top-most to top-bottom, so we need to reverse ociImage History traverse order.
	j := len(manifest.Layers) - 1
	for i := range ociImageHistory {
		history[i] = types.HistoryResultItem{
			Created:    ociImageHistory[lenOciImageHistory-i-1].Created.UnixNano(),
			CreatedBy:  ociImageHistory[lenOciImageHistory-i-1].CreatedBy,
			Author:     ociImageHistory[lenOciImageHistory-i-1].Author,
			Comment:    ociImageHistory[lenOciImageHistory-i-1].Comment,
			EmptyLayer: ociImageHistory[lenOciImageHistory-i-1].EmptyLayer,
			ID:         "<missing>",
			Size:       0,
		}

		// TODO: here we just set imageID of top image layer, we do nothing with the lower image ID, after pouch
		// enables build/commit functionality, we should get local lower image(parent image) layer ID.
		if i == 0 {
			history[i].ID = desc.Digest.String()
		}

		// Note: number of manifest layers should be less than ociImage History messages due to the existence of empty layers.
		// The size of these empty layers should be set to 0 by default.
		if !history[i].EmptyLayer {
			if j < 0 {
				return nil, errors.New("number of manifest layers shouldn't be less than number of non-empty layer in history info")
			}
			info, err := cs.Info(ctx, manifest.Layers[j].Digest)
			if err != nil {
				return nil, err
			}
			history[i].Size = info.Size
			j--
		}
	}
	if j != -1 {
		return nil, errors.New("number of manifest layers shouldn't be greater than number of non-empty layer in history info")
	}
	return history, nil
}

// CheckReference returns image ID and actual reference.
func (mgr *ImageManager) CheckReference(ctx context.Context, idOrRef string) (actualID digest.Digest, actualRef reference.Named, primaryRef reference.Named, err error) {
	var namedRef reference.Named

	namedRef, err = reference.Parse(idOrRef)
	if err != nil {
		return
	}

	// NOTE: we cannot add default registry for the idOrRef directly
	// because the idOrRef maybe short ID or ID. we should run search
	// without addDefaultRegistryIfMissing at first round.
	actualID, actualRef, err = mgr.localStore.Search(namedRef)
	if err != nil {
		if !errtypes.IsNotfound(err) {
			return
		}

		newIDOrRef := addDefaultRegistryIfMissing(idOrRef, mgr.DefaultRegistry, mgr.DefaultNamespace)
		if newIDOrRef == idOrRef {
			return
		}

		// ideally, the err should be nil
		namedRef, err = reference.Parse(newIDOrRef)
		if err != nil {
			return
		}

		actualID, actualRef, err = mgr.localStore.Search(namedRef)
		if err != nil {
			return
		}
	}

	// NOTE: if the actualRef is ID (Named Only) or Digest ID (Tagged Named)
	// the primaryRef is first one of primary reference
	if reference.IsNamedOnly(actualRef) ||
		strings.HasPrefix(actualID.String(), actualRef.String()) {

		refs := mgr.localStore.GetPrimaryReferences(actualID)
		if len(refs) == 0 {
			err = errtypes.ErrNotfound
			logrus.Errorf("one Image ID must have the primary references, but got nothing")
			return
		}

		primaryRef = refs[0]
	} else if primaryRef, err = mgr.localStore.GetPrimaryReference(actualRef); err != nil {
		return
	}
	return
}

// ListReferences returns all references
func (mgr *ImageManager) ListReferences(ctx context.Context, imageID digest.Digest) ([]reference.Named, error) {
	// NOTE: we just keep ctx and error for further expansion
	return mgr.localStore.GetPrimaryReferences(imageID), nil
}

// GetOCIImageConfig returns the image config of OCI
func (mgr *ImageManager) GetOCIImageConfig(ctx context.Context, image string) (ocispec.ImageConfig, error) {
	img, err := mgr.client.GetImage(ctx, image)
	if err != nil {
		return ocispec.ImageConfig{}, err
	}
	ociImage, err := containerdImageToOciImage(ctx, img)
	if err != nil {
		return ocispec.ImageConfig{}, err
	}
	return ociImage.Config, nil
}

// updateLocalStore updates the local store.
func (mgr *ImageManager) updateLocalStore() error {
	ctx, cancel := context.WithTimeout(context.Background(), deadlineLoadImagesAtBootup)
	defer cancel()

	imgs, err := mgr.client.ListImages(ctx)
	if err != nil {
		return err
	}

	for _, img := range imgs {
		if err := mgr.StoreImageReference(ctx, img); err != nil {
			logrus.Warnf("failed to load the image reference into local store: %v", err)
		}
	}
	return nil
}

// StoreImageReference updates image reference in memory store.
func (mgr *ImageManager) StoreImageReference(ctx context.Context, img containerd.Image) error {
	imgCfg, err := img.Config(ctx)
	if err != nil {
		return err
	}

	namedRef, err := reference.Parse(img.Name())
	if err != nil {
		return err
	}

	size, err := img.Size(ctx)
	if err != nil {
		return err
	}

	ociImage, err := containerdImageToOciImage(ctx, img)
	if err != nil {
		return err
	}

	if err := mgr.addReferenceIntoStore(imgCfg.Digest, namedRef, img.Target().Digest); err != nil {
		return err
	}

	mgr.localStore.CacheCtrdImageInfo(imgCfg.Digest, CtrdImageInfo{
		ID:      imgCfg.Digest,
		Size:    size,
		OCISpec: ociImage,
	})
	return nil
}

func (mgr *ImageManager) addReferenceIntoStore(id digest.Digest, ref reference.Named, dig digest.Digest) error {
	// add primary reference as searchable reference
	if err := mgr.localStore.AddReference(id, ref, ref); err != nil {
		return err
	}

	// add Name@Digest as searchable reference if the primary reference is Name:Tag
	if reference.IsNameTagged(ref) {
		// NOTE: The digest reference must be primary reference.
		// If the digest reference has been exist, it means that the
		// same image has been pulled successfully.
		digRef := reference.WithDigest(ref, dig)
		if _, _, err := mgr.localStore.Search(digRef); err != nil {
			if errtypes.IsNotfound(err) {
				return mgr.localStore.AddReference(id, ref, digRef)
			}
		}
	}
	return nil
}

func (mgr *ImageManager) containerdImageToImageInfo(ctx context.Context, id digest.Digest) (types.ImageInfo, error) {
	ctrdImageInfo, err := mgr.localStore.GetCtrdImageInfo(id)
	if err != nil {
		if err == errCtrdImageInfoNotExist {
			return types.ImageInfo{}, pkgerrors.Wrapf(errtypes.ErrNotfound, "failed to get ctrd image info from cache by imageID: %v", id)
		}
		return types.ImageInfo{}, err
	}

	var (
		ociImage    = ctrdImageInfo.OCISpec
		repoTags    = make([]string, 0)
		repoDigests = make([]string, 0)
	)

	for _, ref := range mgr.localStore.GetReferences(ctrdImageInfo.ID) {
		switch ref.(type) {
		case reference.Tagged:
			repoTags = append(repoTags, ref.String())
		case reference.CanonicalDigested:
			repoDigests = append(repoDigests, ref.String())
		}
	}

	return types.ImageInfo{
		Architecture: ociImage.Architecture,
		Config:       getImageInfoConfigFromOciImage(ociImage),
		CreatedAt:    ociImage.Created.Format(utils.TimeLayout),
		ID:           ctrdImageInfo.ID.String(),
		Os:           ociImage.OS,
		RepoDigests:  repoDigests,
		RepoTags:     repoTags,
		RootFS: &types.ImageInfoRootFS{
			Type:   ociImage.RootFS.Type,
			Layers: digestSliceToStringSlice(ociImage.RootFS.DiffIDs),
		},
		Size: ctrdImageInfo.Size,
	}, nil
}

func (mgr *ImageManager) fetchContainerdImage(ctx context.Context, idOrRef string) (containerd.Image, error) {
	_, _, ref, err := mgr.CheckReference(ctx, idOrRef)
	if err != nil {
		return nil, err
	}

	return mgr.client.GetImage(ctx, ref.String())
}

func (mgr *ImageManager) validateTagReference(ref reference.Named) error {
	if _, ok := ref.(reference.Digested); ok {
		return pkgerrors.Wrap(
			errtypes.ErrInvalidParam,
			fmt.Sprintf("target tag reference (%s) cannot contains any digest information", ref.String()),
		)
	}

	// NOTE: we don't allow to use tag to override the existing primary reference.
	pRef, err := mgr.localStore.GetPrimaryReference(ref)
	if err != nil {
		// @fuweid: we should return nil instead of err.
		return nil
	}

	if pRef.String() == ref.String() {
		return pkgerrors.Wrapf(errtypes.ErrInvalidParam, "the tag reference (%s) has been used as reference", ref.String())
	}
	return nil
}

// getManifest gets a manifest from the image for the given platform.
func (mgr *ImageManager) getManifest(ctx context.Context, cs content.Store, img containerd.Image, matcher platforms.MatchComparer) (ocispec.Manifest, error) {
	// layers info
	manifest, err := ctrdmetaimages.Manifest(ctx, cs, img.Target(), matcher)
	if err != nil {
		return ocispec.Manifest{}, err
	}

	// diffIDs info
	diffIDs, err := img.RootFS(ctx)
	if err != nil {
		return ocispec.Manifest{}, err
	}

	if len(manifest.Layers) != len(diffIDs) {
		return ocispec.Manifest{}, errors.New("mismatched image rootfs and manifest layers")
	}

	return manifest, nil
}

func parseTagReference(targetTag string) (reference.Named, error) {
	ref, err := reference.Parse(targetTag)
	if err != nil {
		return nil, pkgerrors.Wrap(errtypes.ErrInvalidParam, err.Error())
	}

	return reference.WithDefaultTagIfMissing(ref), nil
}

func filterReference(filter, ref []string) ([]string, error) {
	if len(filter) == 0 {
		return ref, nil
	}

	var err error
	filteredRefs := make([]string, 0)
	for _, ref := range ref {
		var found bool
		for _, pattern := range filter {
			found, err = filters.FamiliarMatch(pattern, ref)
			if err != nil {
				return []string{}, err
			}
			if found {
				filteredRefs = append(filteredRefs, ref)
				break
			}
		}
	}
	return filteredRefs, nil
}
