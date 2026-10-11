// boeyu-proxy (POC)：面向博育 MDM 接口的 MITM 请求改写，仅作技术验证/演示用途。
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	configPath := flag.String("config", "/config/config.json", "path to config.json")
	flag.Parse()

	if _, err := os.Stat(*configPath); err != nil {
		log.Fatalf("config %s: %v", *configPath, err)
	}
	settings, err := LoadSettings(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	cert, err := EnsureCert(settings.DataDir, settings.CertDomains, settings.CertDays)
	if err != nil {
		log.Fatalf("certificate: %v", err)
	}
	log.Println("boeyu-proxy (POC) starting, config=" + *configPath)
	log.Printf("certificate ready (domains=%v, days=%d) at %s/ca.crt", settings.CertDomains, settings.CertDays, settings.DataDir)

	srv := NewServer(settings, cert, *configPath)
	if err := srv.Start(); err != nil {
		log.Fatalf("start: %v", err)
	}

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
	log.Println("shutting down")
}
