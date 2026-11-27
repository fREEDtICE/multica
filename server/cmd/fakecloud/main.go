// Command fakecloud serves the Route B fake AWS and Alibaba Cloud vendors on
// fixed local ports for browser-based acceptance testing. It is a local
// development/verification facility, not a production component: point
// MULTICA_CLOUD_BUILTIN_CONFIG providers at these endpoints with mode=fake.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/multica-ai/multica/server/internal/cloudruntime/provision"
)

func main() {
	awsAddr := flag.String("aws-addr", "127.0.0.1:18080", "address for the fake AWS EC2/STS endpoint")
	aliyunAddr := flag.String("aliyun-addr", "127.0.0.1:18081", "address for the fake Aliyun ECS/STS endpoint")
	flag.Parse()

	awsHandler, err := provision.NewFakeVendorHandler(provision.ProviderAWS)
	if err != nil {
		log.Fatalf("fake AWS handler: %v", err)
	}
	aliyunHandler, err := provision.NewFakeVendorHandler(provision.ProviderAliyun)
	if err != nil {
		log.Fatalf("fake Aliyun handler: %v", err)
	}

	go func() {
		log.Printf("fake AWS EC2/STS listening on http://%s", *awsAddr)
		if err := http.ListenAndServe(*awsAddr, awsHandler); err != nil {
			log.Fatalf("fake AWS server: %v", err)
		}
	}()
	go func() {
		log.Printf("fake Aliyun ECS/STS listening on http://%s", *aliyunAddr)
		if err := http.ListenAndServe(*aliyunAddr, aliyunHandler); err != nil {
			log.Fatalf("fake Aliyun server: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Print("fakecloud shutting down")
}
