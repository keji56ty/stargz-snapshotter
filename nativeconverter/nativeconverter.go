/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

// Package nativeconverter provides containerd-native converter
// that does not depend on github.com/google/go-containerregistry.
// Most codes except nativeconverter/estargz will be moved to the containerd main repo later.
package nativeconverter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/platforms"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

// New creates a converter.
func New(client *containerd.Client) (*Converter, error) {
	conv := &Converter{
		client: client,
	}
	return conv, nil
}

// Converter provides converter.
type Converter struct {
	client *containerd.Client
}

type convertOpts struct {
	layerConvertFunc ConvertFunc
	docker2oci       bool
	indexConvertFunc ConvertFunc
	platformMC       platforms.MatchComparer
}

// ConvertOpt is an option for Convert()
type ConvertOpt func(*convertOpts) error

// ConvertFunc returns a converted content descriptor.
// When the content was not converted, ConvertContentFunc returns nil.
type ConvertFunc func(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error)

// WithLayerConvertFunc specifies the function that converts layers.
func WithLayerConvertFunc(fn ConvertFunc) ConvertOpt {
	return func(copts *convertOpts) error {
		copts.layerConvertFunc = fn
		return nil
	}
}

// WithDockerToOCI converts Docker media types into OCI ones.
func WithDockerToOCI(v bool) ConvertOpt {
	return func(copts *convertOpts) error {
		copts.docker2oci = true
		return nil
	}
}

// WithPlatform specifies the platform.
// Defaults to all platforms.
func WithPlatform(p platforms.MatchComparer) ConvertOpt {
	return func(copts *convertOpts) error {
		copts.platformMC = p
		return nil
	}
}

// WithIndexConvertFunc specifies the function that converts manifests and index (manifest lists).
// Defaults to DefaultIndexConvertFunc.
func WithIndexConvertFunc(fn ConvertFunc) ConvertOpt {
	return func(copts *convertOpts) error {
		copts.indexConvertFunc = fn
		return nil
	}
}

// Convert converts an image.
func (conv *Converter) Convert(ctx context.Context, dstRef, srcRef string, opts ...ConvertOpt) (*images.Image, error) {
	var copts convertOpts
	for _, o := range opts {
		if err := o(&copts); err != nil {
			return nil, err
		}
	}
	if copts.platformMC == nil {
		copts.platformMC = platforms.All
	}
	if copts.indexConvertFunc == nil {
		copts.indexConvertFunc = DefaultIndexConvertFunc(copts.layerConvertFunc, copts.docker2oci, copts.platformMC)
	}

	ctx, done, err := conv.client.WithLease(ctx)
	if err != nil {
		return nil, err
	}
	defer done(ctx)

	cs := conv.client.ContentStore()
	is := conv.client.ImageService()
	srcImg, err := is.Get(ctx, srcRef)
	if err != nil {
		return nil, err
	}

	dstDesc, err := copts.indexConvertFunc(ctx, cs, srcImg.Target)
	if err != nil {
		return nil, err
	}

	dstImg := srcImg
	dstImg.Name = dstRef
	if dstDesc != nil {
		dstImg.Target = *dstDesc
	}
	var res images.Image
	if dstRef != srcRef {
		_ = is.Delete(ctx, dstRef)
		res, err = is.Create(ctx, dstImg)
	} else {
		res, err = is.Update(ctx, dstImg)
	}
	return &res, err
}

// DefaultIndexConvertFunc is the default convert func.
func DefaultIndexConvertFunc(layerConvertFunc ConvertFunc, docker2oci bool, platformMC platforms.MatchComparer) ConvertFunc {
	c := &defaultConverter{
		layerConvertFunc: layerConvertFunc,
		docker2oci:       docker2oci,
		platformMC:       platformMC,
		diffIDMap:        make(map[digest.Digest]digest.Digest),
	}
	return c.convert
}

type defaultConverter struct {
	layerConvertFunc ConvertFunc
	docker2oci       bool
	platformMC       platforms.MatchComparer
	diffIDMap        map[digest.Digest]digest.Digest // key: old diffID, value: new diffID
	diffIDMapMu      sync.RWMutex
}

// convert dispatches desc.MediaType and calls c.convert{Layer,Manifest,Index,Config}.
//
// Also converts media type if c.docker2oci is set.
func (c *defaultConverter) convert(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
	var (
		newDesc *ocispec.Descriptor
		err     error
	)
	if images.IsLayerType(desc.MediaType) {
		newDesc, err = c.convertLayer(ctx, cs, desc)
	} else if IsManifestType(desc.MediaType) {
		newDesc, err = c.convertManifest(ctx, cs, desc)
	} else if IsIndexType(desc.MediaType) {
		newDesc, err = c.convertIndex(ctx, cs, desc)
	} else if IsConfigType(desc.MediaType) {
		newDesc, err = c.convertConfig(ctx, cs, desc)
	}
	if err != nil {
		return nil, err
	}
	if IsDockerType(desc.MediaType) {
		if c.docker2oci {
			if newDesc == nil {
				newDesc = copyDesc(desc)
			}
			newDesc.MediaType = ConvertDockerMediaTypeToOCI(newDesc.MediaType)
		} else if (newDesc == nil && len(desc.Annotations) != 0) || (newDesc != nil && len(newDesc.Annotations) != 0) {
			// Annotations is supported only on OCI manifest.
			// We need to remove annotations for Docker media types.
			if newDesc == nil {
				newDesc = copyDesc(desc)
			}
			newDesc.Annotations = nil
		}
	}
	logrus.WithField("old", desc).WithField("new", newDesc).Debugf("converted")
	return newDesc, nil
}

func copyDesc(desc ocispec.Descriptor) *ocispec.Descriptor {
	descCopy := desc
	return &descCopy
}

// convertLayer converts image image layers if c.layerConvertFunc is set.
//
// c.layerConvertFunc can be nil, e.g., for converting Docker media types to OCI ones.
func (c *defaultConverter) convertLayer(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
	if c.layerConvertFunc != nil {
		return c.layerConvertFunc(ctx, cs, desc)
	}
	return nil, nil
}

// convertManifest converts image manifests.
//
// - clears `.mediaType` if the target format is OCI
//
// - records diff ID changes in c.diffIDMap
func (c *defaultConverter) convertManifest(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
	var (
		manifest DualManifest
		modified bool
	)
	labels, err := readJSON(ctx, cs, &manifest, desc)
	if err != nil {
		return nil, err
	}
	if IsDockerType(manifest.MediaType) && c.docker2oci {
		manifest.MediaType = ""
		modified = true
	}
	var mu sync.Mutex
	eg, ctx2 := errgroup.WithContext(ctx)
	for i, l := range manifest.Layers {
		i := i
		l := l
		oldDiffID, err := GetDiffID(ctx, cs, l)
		if err != nil {
			return nil, err
		}
		eg.Go(func() error {
			newL, err := c.convert(ctx2, cs, l)
			if err != nil {
				return err
			}
			if newL != nil {
				mu.Lock()
				// update GC labels
				ClearGCLabels(labels, l.Digest)
				labelKey := fmt.Sprintf("containerd.io/gc.ref.content.l.%d", i)
				labels[labelKey] = newL.Digest.String()
				manifest.Layers[i] = *newL
				modified = true
				mu.Unlock()

				// diffID changes if the tar entries were modified.
				// diffID stays same if only the compression type was changed.
				// When diffID changed, add a map entry so that we can update image config.
				newDiffID, err := GetDiffID(ctx, cs, *newL)
				if err != nil {
					return err
				}
				if newDiffID != oldDiffID {
					c.diffIDMapMu.Lock()
					c.diffIDMap[oldDiffID] = newDiffID
					c.diffIDMapMu.Unlock()
				}
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}

	newConfig, err := c.convert(ctx, cs, manifest.Config)
	if err != nil {
		return nil, err
	}
	if newConfig != nil {
		ClearGCLabels(labels, manifest.Config.Digest)
		labels["containerd.io/gc.ref.content.config"] = newConfig.Digest.String()
		manifest.Config = *newConfig
		modified = true
	}

	if modified {
		return writeJSON(ctx, cs, &manifest, desc, labels)
	}
	return nil, nil
}

// convertIndex converts image index.
//
// - clears `.mediaType` if the target format is OCI
//
// - clears manifest entries that do not match c.platformMC
func (c *defaultConverter) convertIndex(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
	var (
		index    DualIndex
		modified bool
	)
	labels, err := readJSON(ctx, cs, &index, desc)
	if err != nil {
		return nil, err
	}
	if IsDockerType(index.MediaType) && c.docker2oci {
		index.MediaType = ""
		modified = true
	}

	newManifests := make([]ocispec.Descriptor, len(index.Manifests))
	newManifestsToBeRemoved := make(map[int]struct{}) // slice index
	var mu sync.Mutex
	eg, ctx2 := errgroup.WithContext(ctx)
	for i, mani := range index.Manifests {
		i := i
		mani := mani
		labelKey := fmt.Sprintf("containerd.io/gc.ref.content.m.%d", i)
		eg.Go(func() error {
			if mani.Platform != nil && !c.platformMC.Match(*mani.Platform) {
				mu.Lock()
				ClearGCLabels(labels, mani.Digest)
				newManifestsToBeRemoved[i] = struct{}{}
				modified = true
				mu.Unlock()
				return nil
			}
			newMani, err := c.convert(ctx2, cs, mani)
			if err != nil {
				return err
			}
			mu.Lock()
			if newMani != nil {
				ClearGCLabels(labels, mani.Digest)
				labels[labelKey] = newMani.Digest.String()
				// NOTE: for keeping manifest order, we specify `i` index explicitly
				newManifests[i] = *newMani
				modified = true
			} else {
				newManifests[i] = mani
			}
			mu.Unlock()
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	if modified {
		var newManifestsClean []ocispec.Descriptor
		for i, m := range newManifests {
			if _, ok := newManifestsToBeRemoved[i]; !ok {
				newManifestsClean = append(newManifestsClean, m)
			}
		}
		index.Manifests = newManifestsClean
		return writeJSON(ctx, cs, &index, desc, labels)
	}
	return nil, nil
}

// convertConfig converts image config contents.
//
// - updates `.rootfs.diff_ids` using c.diffIDMap .
//
// - clears legacy `.config.Image` and `.container_config.Image` fields if `.rootfs.diff_ids` was updated.
func (c *defaultConverter) convertConfig(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
	var (
		cfg      DualConfig
		cfgAsOCI ocispec.Image // read only, used for parsing cfg
		modified bool
	)

	labels, err := readJSON(ctx, cs, &cfg, desc)
	if err != nil {
		return nil, err
	}
	if _, err := readJSON(ctx, cs, &cfgAsOCI, desc); err != nil {
		return nil, err
	}

	if rootfs := cfgAsOCI.RootFS; rootfs.Type == "layers" {
		rootfsModified := false
		c.diffIDMapMu.RLock()
		for i, oldDiffID := range rootfs.DiffIDs {
			if newDiffID, ok := c.diffIDMap[oldDiffID]; ok && newDiffID != oldDiffID {
				rootfs.DiffIDs[i] = newDiffID
				rootfsModified = true
			}
		}
		c.diffIDMapMu.RUnlock()
		if rootfsModified {
			rootfsB, err := json.Marshal(rootfs)
			if err != nil {
				return nil, err
			}
			cfg["rootfs"] = (*json.RawMessage)(&rootfsB)
			modified = true
		}
	}

	if modified {
		// cfg may have dummy value for legacy `.config.Image` and `.container_config.Image`
		// We should clear the ID if we changed the diff IDs.
		if _, err := clearDockerV1DummyID(cfg); err != nil {
			return nil, err
		}
		return writeJSON(ctx, cs, &cfg, desc, labels)
	}
	return nil, nil
}

// clearDockerV1DummyID clears the dummy values for legacy `.config.Image` and `.container_config.Image`.
// Returns true if the cfg was modified.
func clearDockerV1DummyID(cfg DualConfig) (bool, error) {
	var modified bool
	f := func(k string) error {
		if configX, ok := cfg[k]; ok && configX != nil {
			var configField map[string]*json.RawMessage
			if err := json.Unmarshal(*configX, &configField); err != nil {
				return err
			}
			delete(configField, "Image")
			b, err := json.Marshal(configField)
			if err != nil {
				return err
			}
			cfg[k] = (*json.RawMessage)(&b)
			modified = true
		}
		return nil
	}
	if err := f("config"); err != nil {
		return modified, err
	}
	if err := f("container_config"); err != nil {
		return modified, err
	}
	return modified, nil
}

type ObjectWithMediaType struct {
	// MediaType appears on Docker manifests and manifest lists.
	// MediaType does not apper on OCI manifests and index
	MediaType string `json:"mediaType,omitempty"`
}

// DualManifest covers Docker manifest and OCI manifest
type DualManifest struct {
	ocispec.Manifest
	ObjectWithMediaType
}

// DualIndex covers Docker manifest list and OCI index
type DualIndex struct {
	ocispec.Index
	ObjectWithMediaType
}

// DualConfig covers Docker config (v1.0, v1.1, v1.2) and OCI config.
// Unmarshalled as map[string]*json.RawMessage to retain unknown fields on remarshalling.
type DualConfig map[string]*json.RawMessage

func readJSON(ctx context.Context, cs content.Store, x interface{}, desc ocispec.Descriptor) (map[string]string, error) {
	info, err := cs.Info(ctx, desc.Digest)
	if err != nil {
		return nil, err
	}
	labels := info.Labels
	b, err := content.ReadBlob(ctx, cs, desc)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, x); err != nil {
		return nil, err
	}
	return labels, nil
}

func writeJSON(ctx context.Context, cs content.Store, x interface{}, oldDesc ocispec.Descriptor, labels map[string]string) (*ocispec.Descriptor, error) {
	b, err := json.Marshal(x)
	if err != nil {
		return nil, err
	}
	dgst := digest.SHA256.FromBytes(b)
	ref := fmt.Sprintf("converter-write-json-%s", dgst.String())
	w, err := content.OpenWriter(ctx, cs, content.WithRef(ref))
	if err != nil {
		return nil, err
	}
	if err := content.Copy(ctx, w, bytes.NewReader(b), int64(len(b)), dgst, content.WithLabels(labels)); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	newDesc := oldDesc
	newDesc.Size = int64(len(b))
	newDesc.Digest = dgst
	return &newDesc, nil
}

func IsDockerType(mt string) bool {
	return strings.HasPrefix(mt, "application/vnd.docker.")
}

func IsManifestType(mt string) bool {
	switch mt {
	case images.MediaTypeDockerSchema2Manifest, ocispec.MediaTypeImageManifest:
		// no support for schema1 manifest
		return true
	default:
		return false
	}
}

func IsIndexType(mt string) bool {
	switch mt {
	case ocispec.MediaTypeImageIndex, images.MediaTypeDockerSchema2ManifestList:
		return true
	default:
		return false
	}
}

func IsConfigType(mt string) bool {
	switch mt {
	case images.MediaTypeDockerSchema2Config, ocispec.MediaTypeImageConfig:
		return true
	default:
		return false
	}
}

func ConvertDockerMediaTypeToOCI(mt string) string {
	switch mt {
	case images.MediaTypeDockerSchema2ManifestList:
		return ocispec.MediaTypeImageIndex
	case images.MediaTypeDockerSchema2Manifest:
		return ocispec.MediaTypeImageManifest
	case images.MediaTypeDockerSchema2LayerGzip:
		return ocispec.MediaTypeImageLayerGzip
	case images.MediaTypeDockerSchema2LayerForeignGzip:
		return ocispec.MediaTypeImageLayerNonDistributableGzip
	case images.MediaTypeDockerSchema2Layer:
		return ocispec.MediaTypeImageLayer
	case images.MediaTypeDockerSchema2LayerForeign:
		return ocispec.MediaTypeImageLayerNonDistributable
	case images.MediaTypeDockerSchema2Config:
		return ocispec.MediaTypeImageConfig
	default:
		return mt
	}
}

const LabelUncompressed = "containerd.io/uncompressed"

func GetDiffID(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (digest.Digest, error) {
	switch desc.MediaType {
	case
		images.MediaTypeDockerSchema2Layer,
		ocispec.MediaTypeImageLayer,
		images.MediaTypeDockerSchema2LayerForeign,
		ocispec.MediaTypeImageLayerNonDistributable:
		return desc.Digest, nil
	}
	info, err := cs.Info(ctx, desc.Digest)
	if err != nil {
		return "", err
	}
	v, ok := info.Labels[LabelUncompressed]
	if !ok {
		return "", errors.Wrapf(errdefs.ErrNotFound, "content %s does not have label %q",
			desc.Digest.String(), LabelUncompressed)
	}
	return digest.Parse(v)
}

func ClearGCLabels(labels map[string]string, dgst digest.Digest) {
	for k, v := range labels {
		if v == dgst.String() && strings.HasPrefix(k, "containerd.io/gc.ref.content") {
			delete(labels, k)
		}
	}
}