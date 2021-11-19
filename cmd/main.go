package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"

	"github.com/aojea/kloudflarelb/pkg/cloudflared"
	"github.com/aojea/kloudflarelb/pkg/config"
	"github.com/aojea/kloudflarelb/pkg/loadbalancer"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/klog/v2"
)

func main() {
	var kubeconfig *string

	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}

	c := config.Config{}
	flag.StringVar(&c.Domain, "domain", "", "domain associated to the tunnel")
	flag.StringVar(&c.TunnelID, "tunnelID", "", "cloudlfared tunnel <name/uuid>")
	flag.StringVar(&c.CredentialsFile, "credentials-file", "", "cloudflare credentials file")

	klog.InitFlags(nil)
	flag.Parse()

	// use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err.Error())
	}

	// create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	informer := informers.NewSharedInformerFactory(clientset, 0)
	lbController := loadbalancer.NewController(
		c,
		clientset,
		informer.Core().V1().Services(),
	)

	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	klog.Info("Starting informer")
	informer.Start(ctx.Done())
	go lbController.Run(1, ctx.Done())

	cloudflare := cloudflared.NewFromConfig(c)
	klog.Info("Starting cloudflared daemon")
	err = cloudflare.Run(ctx)
	if err != nil {
		klog.Errorf("Error running cloudflared: %v", err)
		os.Exit(1)
	}
	os.Exit(0)
}
