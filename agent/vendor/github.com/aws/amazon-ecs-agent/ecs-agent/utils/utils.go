// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.
package utils

import (
	"reflect"
	"regexp"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"golang.org/x/exp/constraints"
)

const httpsPrefix = "https://"

var schemeRegex = regexp.MustCompile(`^(http|https)://`)

func ZeroOrNil(obj interface{}) bool {
	value := reflect.ValueOf(obj)
	if !value.IsValid() {
		return true
	}
	if obj == nil {
		return true
	}
	switch value.Kind() {
	case reflect.Slice, reflect.Array, reflect.Map:
		return value.Len() == 0
	}
	zero := reflect.Zero(reflect.TypeOf(obj))
	if !value.Type().Comparable() {
		return false
	}
	if obj == zero.Interface() {
		return true
	}
	return false
}

// Uint16SliceToStringSlice converts a slice of type uint16 to a slice of type
// string. It uses strconv.Itoa on each element
func Uint16SliceToStringSlice(slice []uint16) []string {
	stringSlice := make([]string, len(slice))
	for i, el := range slice {
		str := strconv.Itoa(int(el))
		stringSlice[i] = str
	}
	return stringSlice
}

// Int32PtrToIntPtr converts a *int32 to *int.
func Int32PtrToIntPtr(int32ptr *int32) *int {
	if int32ptr == nil {
		return nil
	}
	return aws.Int(int(aws.ToInt32(int32ptr)))
}

// Int64PtrToInt32Ptr converts a *int64 to *int32.
func Int64PtrToInt32Ptr(int64ptr *int64) *int32 {
	if int64ptr == nil {
		return nil
	}
	return aws.Int32(int32(aws.ToInt64(int64ptr)))
}

// MaxNum returns the maximum value between two numbers.
func MaxNum[T constraints.Integer | constraints.Float](a, b T) T {
	if a > b {
		return a
	}
	return b
}

// If the URL doesn't start with "http://" or "https://",
// prepends "https://" to the URL.
// Empty strings are returned as-is without modification
func AddScheme(endpoint string) string {
	if endpoint == "" {
		return endpoint
	}

	if schemeRegex.MatchString(endpoint) {
		return endpoint
	}
	return httpsPrefix + endpoint
}
