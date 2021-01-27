package piifilterprocessor

import (
	"context"
	"errors"
	"fmt"

	"github.com/hypertrace/collector/processors/piifilterprocessor/filters"
	"github.com/hypertrace/collector/processors/piifilterprocessor/filters/cookie"
	"github.com/hypertrace/collector/processors/piifilterprocessor/filters/json"
	"github.com/hypertrace/collector/processors/piifilterprocessor/filters/keyvalue"
	"github.com/hypertrace/collector/processors/piifilterprocessor/filters/regexmatcher"
	"github.com/hypertrace/collector/processors/piifilterprocessor/filters/urlencoded"
	"go.opentelemetry.io/collector/consumer/pdata"
	"go.opentelemetry.io/collector/processor/processorhelper"
	"go.uber.org/zap"
)

var _ processorhelper.TProcessor = (*piiFilterProcessor)(nil)

type piiFilterProcessor struct {
	logger  *zap.Logger
	filters []filters.Filter
}

func toRegex(es []PiiElement) []regexmatcher.Regex {
	var rs []regexmatcher.Regex

	for _, e := range es {
		rs = append(rs, regexmatcher.Regex{
			Pattern:        e.Regex,
			RedactStrategy: e.RedactStrategy,
			FQN:            e.FQN,
		})
	}

	return rs
}

func newPIIFilterProcessor(
	logger *zap.Logger,
	cfg *Config,
) (*piiFilterProcessor, error) {
	matcher, err := regexmatcher.NewMatcher(
		toRegex(cfg.KeyRegExs),
		toRegex(cfg.ValueRegExs),
		cfg.RedactStrategy,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create regex matcher: %v", err)
	}

	var fs = []filters.Filter{
		keyvalue.NewFilter(matcher),
		cookie.NewFilter(matcher),
		urlencoded.NewFilter(matcher),
		json.NewFilter(matcher),
	}

	return &piiFilterProcessor{
		logger:  logger,
		filters: fs,
	}, nil
}

func (p *piiFilterProcessor) ProcessTraces(_ context.Context, td pdata.Traces) (pdata.Traces, error) {
	rss := td.ResourceSpans()
	for i := 0; i < rss.Len(); i++ {
		rs := rss.At(i)

		ilss := rs.InstrumentationLibrarySpans()
		for j := 0; j < ilss.Len(); j++ {
			ils := ilss.At(j)
			spans := ils.Spans()
			for k := 0; k < spans.Len(); k++ {
				span := spans.At(k)

				span.Attributes().ForEach(func(key string, value pdata.AttributeValue) {
					for _, filter := range p.filters {
						if isRedacted, err := filter.RedactAttribute(key, value); err != nil {
							if errors.Is(err, filters.ErrUnprocessableValue) {
								// this should be debug when we figure out how to configure the log level
								p.logger.Sugar().Debugf("failed to apply filter %q to attribute with key %q. Unsuitable value.", filter.Name(), key)
							} else {
								p.logger.Sugar().Errorf("failed to apply filter %q to attribute with key %q: %v", filter.Name(), key, err)
							}
						} else if isRedacted {
							// if an attribute is redacted by one filter we don't want to process
							// it again.
							p.logger.Sugar().Debugf("attribute with key %q redacted by filter %q", key, filter.Name())
							break
						}
					}
				})
			}
		}
	}

	return td, nil
}
