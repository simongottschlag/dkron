package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConvertStringSliceToAzureSDK(t *testing.T) {
	input := []string{"a", "b", "c"}
	output := convertStringSliceToAzureSDK(input)
	var convertedOutput []string
	for i, _ := range input {
		convertedOutput = append(convertedOutput, *output[i])
	}

	require.Equal(t, input, convertedOutput)
}
