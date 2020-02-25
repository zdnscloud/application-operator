package main

import (
	"github.com/zdnscloud/cement/log"
	"github.com/zdnscloud/gok8s/cache"
	"github.com/zdnscloud/gok8s/client"
	"github.com/zdnscloud/gok8s/client/config"
	gok8sctrl "github.com/zdnscloud/gok8s/controller"
	"github.com/zdnscloud/gok8s/predicate"

	appv1beta1 "github.com/zdnscloud/application-operator/pkg/apis/app/v1beta1"
	"github.com/zdnscloud/application-operator/pkg/controller"
)

func main() {
	log.InitLogger("debug")
	cfg, err := config.GetConfig()
	if err != nil {
		log.Fatalf("get config failed: %s", err.Error())
	}

	stop := make(chan struct{})
	defer close(stop)

	c, err := cache.New(cfg, cache.Options{})
	if err != nil {
		log.Fatalf("create cache failed: %s", err.Error())
	}
	go c.Start(stop)

	c.WaitForCacheSync(stop)

	var options client.Options
	options.Scheme = client.GetDefaultScheme()
	appv1beta1.AddToScheme(options.Scheme)
	kubeClient, err := client.New(cfg, options)
	if err != nil {
		log.Fatalf("create k8s client failed: %s", err.Error())
	}

	ctrl := gok8sctrl.New("appController", c, options.Scheme)
	ctrl.Watch(&appv1beta1.Application{})
	m, err := controller.New(c, kubeClient)
	if err != nil {
		log.Fatalf("create application manager failed: %s", err.Error())
	}

	ctrl.Start(stop, m, predicate.NewIgnoreUnchangedUpdate())
}
