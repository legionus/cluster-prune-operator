package imagestream

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/golang/glog"

	"k8s.io/apimachinery/pkg/api/meta"
	metaapi "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/pager"

	imageapi "github.com/openshift/api/image/v1"
	imageclset "github.com/openshift/client-go/image/clientset/versioned"

	pruneapi "github.com/openshift/cluster-prune-operator/pkg/apis/prune/v1alpha1"
	"github.com/openshift/cluster-prune-operator/pkg/reference"
	"github.com/openshift/cluster-prune-operator/pkg/imagereference"
)

type Pruner struct {
	Config      *pruneapi.PruneServiceImages
	ImageClient *imageclset.Clientset
	References  *imagereference.References
}

func (p *Pruner) pruneImageStream(ctx context.Context, is *imageapi.ImageStream) {
	isChanged := false

	timeThreshold := time.Now()

	if p.Config.KeepTagYoungerThan != nil {
		timeThreshold.Add(-*p.Config.KeepTagYoungerThan)
	}

	namedRef := &reference.Image{
		Name:      is.GetName(),
		Namespace: is.GetNamespace(),
	}

	glog.Infof("prune imagestream %s", namedRef.String())

	prunableTags := map[*imageapi.NamedTagEventList]struct{}{}

	for i := len(is.Status.Tags) - 1; i != 0; i-- {
		istag := is.Status.Tags[i]
		namedRef.Tag = istag.Tag

		prunableTagItems := map[*imageapi.TagEvent]struct{}{}

		for j, revtag := range istag.Items {
			namedRef.ID = istag.Items[j].Image

			if p.Config.KeepTagRevisions != nil && j < *p.Config.KeepTagRevisions {
				if glog.V(5) {
					glog.Infof("ignore imagestream tag revision due keep-tag-revisions limit: %s", namedRef.String())
				}
				continue
			}

			if revtag.Created.After(timeThreshold) {
				if glog.V(5) {
					glog.Infof("ignore imagestream tag revision due age limit: %s", namedRef.String())
				}
				continue
			}

			namedRef.ID = revtag.Image

			if p.References.IsExist(namedRef.String()) {
				continue
			}

			prunableTagItems[&istag.Items[j]] = struct{}{}
		}

		namedRef.Tag = istag.Tag
		namedRef.ID = ""

		if p.References.IsExist(namedRef.String()) && len(prunableTagItems) == len(istag.Items) {
			// If there is a reference to the tag without a revision,
			// then we must leave at least one last revision.
			delete(prunableTagItems, &istag.Items[0])
		}

		switch len(prunableTagItems) {
		case 0:
			// Nothing to do.
			if glog.V(5) {
				glog.Infof("imagestream tag %s is not touched, %d revisions is found", namedRef.String(), len(istag.Items))
			}
			continue
		case len(istag.Items):
			// Tags without revisions do not make sense.
			if glog.V(5) {
				glog.Infof("remove all revisions in %s", namedRef.String())
			}

			prunableTags[&is.Status.Tags[i]] = struct{}{}
			isChanged = true

			continue
		}

		var tagItems []imageapi.TagEvent

		for j := range istag.Items {
			if _, ok := prunableTagItems[&istag.Items[j]]; !ok {
				tagItems = append(tagItems, istag.Items[j])
			} else {
				namedRef.Tag = istag.Tag
				namedRef.ID = istag.Items[j].Image
				glog.Infof("prune tag revision at position %s: %s", j, namedRef.String())
			}
		}

		is.Status.Tags[i].Items = tagItems
		isChanged = true
	}

	if len(prunableTags) > 0 {
		namedRef.ID = ""

		if len(prunableTags) == len(is.Status.Tags) {
			// TODO(legion) leave empty imagestream ?
		}

		var tags []imageapi.NamedTagEventList

		for i := range is.Status.Tags {
			if _, ok := prunableTags[&is.Status.Tags[i]]; !ok {
				tags = append(tags, is.Status.Tags[i])
			} else {
				namedRef.Tag = is.Status.Tags[i].Tag
				glog.Infof("prune tag %s", namedRef.String())
			}
		}

		is.Status.Tags = tags
		isChanged = true
	}

	namedRef.Tag = ""
	namedRef.ID = ""

	if !isChanged {
		glog.Infof("prune imagestream %s done", namedRef.String())
		return
	}

	// TODO(legion) update imagestream
	glog.Infof("update %s", namedRef.String())
}

func (p *Pruner) Run(ctx context.Context) error {
	if p.References == nil {
		return nil
	}

	glog.Infof("imagestreams pruning ...")

	listObj, err := pager.New(func(ctx context.Context, opts metaapi.ListOptions) (runtime.Object, error) {
		select {
		case <-ctx.Done():
			return nil, nil
		default:
		}
		return p.ImageClient.Image().ImageStreams(metaapi.NamespaceAll).List(opts)
	}).List(ctx, metaapi.ListOptions{})

	if err != nil {
		return fmt.Errorf("unable to fetch imagestreams: %s", err)
	}

	var wg sync.WaitGroup

	err = meta.EachListItem(listObj, func(o runtime.Object) error {
		is := o.(*imageapi.ImageStream)

		wg.Add(1)
		go func(is *imageapi.ImageStream) {
			defer wg.Done()
			p.pruneImageStream(ctx, is)
		}(is)

		return nil
	})

	if err != nil {
		return fmt.Errorf("unable to process imagestreams: %s", err)
	}

	wg.Wait()

	glog.Infof("imagestreams pruning done")
	return nil
}
