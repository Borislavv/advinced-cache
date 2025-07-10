package model

import (
	"bytes"
	"compress/gzip"
	"github.com/Borislavv/advanced-cache/pkg/config"
	"github.com/stretchr/testify/require"
	"io"
	"testing"
)

func TestEntryPayloadRoundTrip(t *testing.T) {
	rule := &config.Rule{
		Gzip: config.Gzip{
			Enabled:   true,
			Threshold: 0, // Форсируем gzip даже для маленького тела
		},
	}

	// Исходные данные
	path := []byte("/test/path")
	query := []byte("?foo=bar&baz=qux")
	queryHeaders := [][2][]byte{
		{[]byte("X-Q-1"), []byte("v1")},
		{[]byte("X-Q-2"), []byte("v2")},
	}
	headers := [][2][]byte{
		{[]byte("Content-Type"), []byte("application/json")},
		{[]byte("X-Resp"), []byte("yes")},
	}
	body := []byte(`{"foo":"bar","baz":"qux"}`)
	status := 200

	// === 1) Создаём Entry и упаковываем
	e := (&Entry{rule: rule}).Init()
	e.SetPayload(path, query, queryHeaders, headers, body, status)

	// === 2) Распаковываем
	p1, q1, qh1, h1, b1, s1, release, err := e.Payload()
	require.NoError(t, err)
	defer release()

	// === 3) Проверяем значения
	require.Equal(t, path, p1)
	require.Equal(t, query, q1)
	require.Equal(t, status, s1)
	require.Equal(t, body, b1)

	require.Equal(t, queryHeaders, qh1)
	require.Equal(t, headers, h1)

	// === 4) Повторно запаковываем, используя распакованные данные
	e2 := (&Entry{rule: rule}).Init()
	e2.SetPayload(p1, q1, qh1, h1, b1, s1)

	// === 5) И снова распаковываем
	p2, q2, qh2, h2, b2, s2, release2, err := e2.Payload()
	require.NoError(t, err)
	defer release2()

	require.Equal(t, p1, p2)
	require.Equal(t, q1, q2)
	require.Equal(t, s1, s2)
	require.Equal(t, b1, b2)
	require.Equal(t, qh1, qh2)
	require.Equal(t, h1, h2)

	// === 6) Проверим побайтово, что всё сохраняется и в сжатой форме
	if e2.IsCompressed() {
		// Распакуй и проверь вручную
		gr, err := gzip.NewReader(bytes.NewReader(e2.PayloadBytes()))
		require.NoError(t, err)
		defer gr.Close()

		raw, err := io.ReadAll(gr)
		require.NoError(t, err)

		gr1, err := gzip.NewReader(bytes.NewReader(e.PayloadBytes()))
		require.NoError(t, err)
		defer gr1.Close()

		raw1, err := io.ReadAll(gr1)
		require.NoError(t, err)

		require.Equal(t, raw1, raw, "compressed raw payloads must match")
	}
}
