package commontest

import (
	"testing"

	"github.com/squareup/pranadb/common"
	"github.com/stretchr/testify/require"
)

func TestByteSliceMap(t *testing.T) {
	bsl := common.NewByteSliceMap()
	k := []byte("somekey")
	v := []byte("somevalue")
	bsl.Put(k, v)

	v2, ok := bsl.Get(k)

	require.True(t, ok)
	require.Equal(t, "somevalue", string(v2))

	_, ok = bsl.Get([]byte("not_exists"))
	require.False(t, ok)
}

func TestByteSliceToStringZeroCopy(t *testing.T) {
	b1 := []byte("string1")
	b2 := []byte("")

	s1 := common.ByteSliceToStringZeroCopy(b1)
	require.Equal(t, "string1", s1)
	s2 := common.ByteSliceToStringZeroCopy(b2)
	require.Equal(t, "", s2)
}

func TestStringToByteSliceZeroCopy(t *testing.T) {
	s1 := "string1"
	s2 := ""

	b1 := common.StringToByteSliceZeroCopy(s1)
	require.Equal(t, "string1", string(b1))
	b2 := common.StringToByteSliceZeroCopy(s2)
	require.Equal(t, "", string(b2))
}
