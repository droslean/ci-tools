package main

import (
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"

	"github.com/ghodss/yaml"
	"github.com/sirupsen/logrus"

	imagev1 "github.com/openshift/api/image/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openshift/ci-tools/pkg/api"
	"github.com/openshift/ci-tools/pkg/config"
	"github.com/openshift/ci-tools/pkg/promotion"
)

type options struct {
	configDir       string
	branch          string
	fromImagestream string
	toImageStream   string
	toNamespace     string
}

func gatherOptions() options {
	o := options{}
	fs := flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	fs.StringVar(&o.configDir, "config-path", "", "Path to directory containing ci-operator configurations")
	fs.StringVar(&o.branch, "branch", "", "")

	fs.StringVar(&o.fromImagestream, "from-imagestream", "", "")
	fs.StringVar(&o.toImageStream, "to-imagestream", "", "")

	fs.StringVar(&o.toNamespace, "to-namespace", "", "")

	fs.Parse(os.Args[1:])
	return o
}

func (o *options) validate() error {
	if len(o.configDir) == 0 {
		return errors.New("--config-path is not defined")
	}
	if len(o.branch) == 0 {
		return errors.New("--branch is not defined")
	}
	if len(o.fromImagestream) == 0 {
		return errors.New("--from-imagestream is not defined")
	}
	if len(o.toImageStream) == 0 {
		return errors.New("--to-imagestream is not defined")
	}
	if len(o.toNamespace) == 0 {
		return errors.New("--to-namespace is not defined")
	}
	return nil
}

func main() {
	o := gatherOptions()
	if err := o.validate(); err != nil {
		logrus.WithError(err).Fatal("Invalid option")
	}

	var tags []imagev1.TagReference

	callback := func(rbc *api.ReleaseBuildConfiguration, repoInfo *config.Info) error {
		logger := logrus.WithFields(logrus.Fields{"org": repoInfo.Org, "repo": repoInfo.Repo, "branch": repoInfo.Branch})

		if !promotion.BuildsOfficialImages(rbc) {
			return nil
		}

		if rbc.PromotionConfiguration != nil && !rbc.PromotionConfiguration.Disabled {
			if repoInfo.Branch == o.branch {
				logger.Info("Processing...")
				for _, image := range rbc.Images {

					imageName := string(api.PipelineImageStreamTagReference(image.To))
					for _, excluded := range rbc.PromotionConfiguration.ExcludedImages {
						if excluded == imageName {
							logger.WithField("image", imageName).Warning("Exluded image")
							return nil
						}
					}

					tag := imagev1.TagReference{

						Name: imageName,
						From: &corev1.ObjectReference{
							Kind: "DockerImage",
							Name: fmt.Sprintf("%s:%s", o.fromImagestream, imageName),
						},
						ImportPolicy: imagev1.TagImportPolicy{Scheduled: true},
					}

					tags = append(tags, tag)

				}
			}
		}

		return nil
	}

	if err := config.OperateOnCIOperatorConfigDir(o.configDir, callback); err != nil {
		logrus.WithError(err).Fatal("error while generating the ci-operator configuration files")
	}

	newIs := &imagev1.ImageStream{
		TypeMeta:   metav1.TypeMeta{Kind: "ImageStream", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: o.toImageStream, Namespace: o.toNamespace},
		Spec: imagev1.ImageStreamSpec{
			LookupPolicy: imagev1.ImageLookupPolicy{Local: true},
			Tags:         tags,
		},
	}

	b, err := yaml.Marshal(newIs)
	if err != nil {
		os.Exit(1)
	}

	if err := ioutil.WriteFile(fmt.Sprintf("%s-is.yaml", o.toImageStream), b, 0775); err != nil {
		os.Exit(1)
	}

}
