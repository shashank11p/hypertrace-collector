package main

import (
	"fmt"
	"log"

	"go.opencensus.io/stats/view"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenterror"
	"go.opentelemetry.io/collector/service"
	"go.opentelemetry.io/collector/service/defaultcomponents"

	"github.com/hypertrace/collector/processors/tenantidprocessor"
)

func main() {
	if err := registerMetricViews(); err != nil {
		log.Fatal(err)
	}

	factories, err := components()
	if err != nil {
		log.Fatalf("failed to build default components: %v", err)
	}

	info := component.ApplicationStartInfo{
		ExeName:  "collector",
		LongName: "Hypertrace Collector",
		Version:  Version,
		GitHash:  GitHash,
	}

	if err := run(service.Parameters{ApplicationStartInfo: info, Factories: factories}); err != nil {
		log.Fatal(err)
	}
}

func components() (component.Factories, error) {
	var errs []error
	factories, err := defaultcomponents.Components()
	if err != nil {
		return component.Factories{}, err
	}

	processors := []component.ProcessorFactory{
		tenantidprocessor.NewFactory(),
	}
	for _, pr := range factories.Processors {
		processors = append(processors, pr)
	}
	factories.Processors, err = component.MakeProcessorFactoryMap(processors...)
	if err != nil {
		errs = append(errs, err)
	}

	return factories, componenterror.CombineErrors(errs)
}

func run(params service.Parameters) error {
	app, err := service.New(params)
	if err != nil {
		return fmt.Errorf("failed to construct the application: %w", err)
	}

	err = app.Run()
	if err != nil {
		return fmt.Errorf("application run finished with error: %w", err)
	}

	return nil
}

func registerMetricViews() error {
	views := tenantidprocessor.MetricViews()
	return view.Register(views...)
}
