package main

import (
	"context"
	"net/http"
	"testing"

	"rest_without_return_type/internal/gen/pkg/servers/pingpong"
)

const applicationConfig = `---
genCode:
  upstream:
    contextTimeout: "0.5s"
`

func TestRestWithoutReturnType(t *testing.T) {
	t.Parallel()
	pingpongTester := pingpong.NewTestServer(t, context.Background(), createService, applicationConfig)
	defer pingpongTester.Close()

	pingpongTester.GetPing1(12345).
		ExpectResponseCode(http.StatusOK).
		ExpectResponseBody(pingpong.Pong{12345}).
		Send()

	pingpongTester.GetPing2(12345).
		ExpectResponseCode(http.StatusOK).
		ExpectResponseBody(pingpong.Pong{12345}).
		Send()

	pingpongTester.GetPingtimeout(12345).
		ExpectResponseCode(http.StatusInternalServerError).
		Send()
}
