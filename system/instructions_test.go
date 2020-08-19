package system

import (
	"bytes"
	"testing"

	solana "github.com/dfuse-io/solana-go"
	"github.com/lunixbochs/struc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSystemInstructions(t *testing.T) {
	ins1 := NewInstruction(&CreateAccount{
		Lamports: 125,
		Space:    120,
		Owner:    solana.MustPublicKeyFromBase58("4JuGp6UkTewQXG1tJpYY1dxW1H9yS6sSeCDc1FSdWKNR"),
	})

	buf := &bytes.Buffer{}
	err := struc.Pack(buf, ins1)
	require.NoError(t, err)
	assert.Equal(t, []byte{0, 1, 2, 3}, buf.Bytes())

	// tests := []struct{
	//     name string
	//     input string
	//     expect string
	// }{
	//     {
	//         name: "name",
	//         input: "input",
	//         expect: "expect",
	//     },
	// }

	// for _, test := range tests {
	//     t.Run(test.name, func(t *testing.T) {
	//         res := (test.in)
	//         assert.Equal(t, test.expect, res)
	//     })
	// }
}