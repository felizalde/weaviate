//                           _       _
// __      _____  __ ___   ___  __ _| |_ ___
// \ \ /\ / / _ \/ _` \ \ / / |/ _` | __/ _ \
//  \ V  V /  __/ (_| |\ V /| | (_| | ||  __/
//   \_/\_/ \___|\__,_| \_/ |_|\__,_|\__\___|
//
//  Copyright © 2016 - 2023 Weaviate B.V. All rights reserved.
//
//  CONTACT: hello@weaviate.io
//

package modgenerativeopenai

import (
	"context"
	"net/http"
	"os"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/weaviate/weaviate/entities/modulecapabilities"
	"github.com/weaviate/weaviate/entities/moduletools"
	generativeadditional "github.com/weaviate/weaviate/modules/generative-openai/additional"
	generativeadditionalgenerate "github.com/weaviate/weaviate/modules/generative-openai/additional/generate"
	"github.com/weaviate/weaviate/modules/generative-openai/clients"
	"github.com/weaviate/weaviate/modules/generative-openai/ent"
)

const Name = "generative-openai"

func New() *GenerativeOpenAIModule {
	return &GenerativeOpenAIModule{}
}

type GenerativeOpenAIModule struct {
	generative                   generativeClient
	graphqlProvider              modulecapabilities.GraphQLArguments
	searcher                     modulecapabilities.DependencySearcher
	additionalPropertiesProvider modulecapabilities.AdditionalProperties
	nearTextDependencies         []modulecapabilities.Dependency
	askTextTransformer           modulecapabilities.TextTransform
}

type generativeClient interface {
	Generate(ctx context.Context, text, question, language string, cfg moduletools.ClassConfig) (*ent.GenerateResult, error)
	MetaInfo() (map[string]interface{}, error)
}

func (m *GenerativeOpenAIModule) Name() string {
	return Name
}

func (m *GenerativeOpenAIModule) Type() modulecapabilities.ModuleType {
	return modulecapabilities.Text2Text
}

func (m *GenerativeOpenAIModule) Init(ctx context.Context,
	params moduletools.ModuleInitParams,
) error {
	if err := m.initAdditional(ctx, params.GetLogger()); err != nil {
		return errors.Wrap(err, "init q/a")
	}

	return nil
}

func (m *GenerativeOpenAIModule) initAdditional(ctx context.Context,
	logger logrus.FieldLogger,
) error {
	apiKey := os.Getenv("OPENAI_APIKEY")

	client := clients.New(apiKey, logger)

	m.generative = client

	generateProvider := generativeadditionalgenerate.New(m.generative)
	m.additionalPropertiesProvider = generativeadditional.New(generateProvider)

	return nil
}

func (m *GenerativeOpenAIModule) RootHandler() http.Handler {
	// TODO: remove once this is a capability interface
	return nil
}

func (m *GenerativeOpenAIModule) MetaInfo() (map[string]interface{}, error) {
	return m.generative.MetaInfo()
}

func (m *GenerativeOpenAIModule) AdditionalProperties() map[string]modulecapabilities.AdditionalProperty {
	return m.additionalPropertiesProvider.AdditionalProperties()
}

// verify we implement the modules.Module interface
var (
	_ = modulecapabilities.Module(New())
	_ = modulecapabilities.AdditionalProperties(New())
	_ = modulecapabilities.MetaProvider(New())
)