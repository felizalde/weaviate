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

package distancer

import "github.com/pkg/errors"

var l2SquaredImpl func(a, b []float32) float32 = func(a, b []float32) float32 {
	var sum float32

	for i := range a {
		sum += l2SquaredStepImpl(a[i], b[i])
	}

	return sum
}

var l2SquaredStepImpl func(a, b float32) float32 = func(a, b float32) float32 {
	diff := a - b
	return diff * diff
}

type L2Squared struct {
	a []float32
}

func (l L2Squared) Distance(b []float32) (float32, bool, error) {
	if len(l.a) != len(b) {
		return 0, false, errors.Errorf("vector lengths don't match: %d vs %d",
			len(l.a), len(b))
	}

	return l2SquaredImpl(l.a, b), true, nil
}

type L2SquaredProvider struct{}

func NewL2SquaredProvider() L2SquaredProvider {
	return L2SquaredProvider{}
}

func (l L2SquaredProvider) SingleDist(a, b []float32) (float32, bool, error) {
	if len(a) != len(b) {
		return 0, false, errors.Errorf("vector lengths don't match: %d vs %d",
			len(a), len(b))
	}

	return l2SquaredImpl(a, b), true, nil
}

func (l L2SquaredProvider) Type() string {
	return "l2-squared"
}

func (l L2SquaredProvider) New(a []float32) Distancer {
	return &L2Squared{a: a}
}

func (l L2SquaredProvider) Step(x, y float32) float32 {
	return l2SquaredStepImpl(x, y)
}

func (l L2SquaredProvider) Wrap(x float32) float32 {
	return x
}
