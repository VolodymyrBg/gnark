// Copyright 2020 ConsenSys Software Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package plonk

import (
	"crypto/sha256"
	"math/big"
	"runtime"
	"time"

	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr/iop"

	curve "github.com/consensys/gnark-crypto/ecc/bn254"

	"github.com/consensys/gnark-crypto/ecc/bn254/fr/kzg"

	"github.com/consensys/gnark-crypto/ecc/bn254/fr/fft"

	bn254witness "github.com/consensys/gnark/internal/backend/bn254/witness"

	cs "github.com/consensys/gnark/constraint/bn254"

	fiatshamir "github.com/consensys/gnark-crypto/fiat-shamir"
	"github.com/consensys/gnark/backend"
	"github.com/consensys/gnark/internal/utils"
	"github.com/consensys/gnark/logger"
)

type Proof struct {

	// Commitments to the solution vectors
	LRO [3]kzg.Digest

	// Commitment to Z, the permutation polynomial
	Z kzg.Digest

	// Commitments to h1, h2, h3 such that h = h1 + Xh2 + X**2h3 is the quotient polynomial
	H [3]kzg.Digest

	// Batch opening proof of h1 + zeta*h2 + zeta**2h3, linearizedPolynomial, l, r, o, s1, s2
	BatchedProof kzg.BatchOpeningProof

	// Opening proof of Z at zeta*mu
	ZShiftedOpening kzg.OpeningProof
}

// Prove from the public data
func Prove(spr *cs.SparseR1CS, pk *ProvingKey, fullWitness bn254witness.Witness, opt backend.ProverConfig) (*Proof, error) {

	log := logger.Logger().With().Str("curve", spr.CurveID().String()).Int("nbConstraints", len(spr.Constraints)).Str("backend", "plonk").Logger()
	start := time.Now()
	// pick a hash function that will be used to derive the challenges
	hFunc := sha256.New()

	// create a transcript manager to apply Fiat Shamir
	fs := fiatshamir.NewTranscript(hFunc, "gamma", "beta", "alpha", "zeta")

	// result
	proof := &Proof{}

	// compute the constraint system solution
	var solution []fr.Element
	var err error
	if solution, err = spr.Solve(fullWitness, opt); err != nil {
		if !opt.Force {
			return nil, err
		} else {
			// we need to fill solution with random values
			var r fr.Element
			_, _ = r.SetRandom()
			for i := len(spr.Public) + len(spr.Secret); i < len(solution); i++ {
				solution[i] = r
				r.Double(&r)
			}
		}
	}

	// query l, r, o in Lagrange basis, not blinded
	evaluationLDomainSmall, evaluationRDomainSmall, evaluationODomainSmall := evaluateLROSmallDomain(spr, pk, solution)

	lagReg := iop.Form{Basis: iop.Lagrange, Layout: iop.Regular}
	liop := iop.Polynomial{Coefficients: evaluationLDomainSmall, Form: lagReg}
	riop := iop.Polynomial{Coefficients: evaluationRDomainSmall, Form: lagReg}
	oiop := iop.Polynomial{Coefficients: evaluationODomainSmall, Form: lagReg}
	wliop := liop.WrapMe(0)
	wriop := riop.WrapMe(0)
	woiop := oiop.WrapMe(0)
	wliop.ToCanonical(wliop, &pk.Domain[0]).ToRegular(wliop)
	wriop.ToCanonical(wriop, &pk.Domain[0]).ToRegular(wriop)
	woiop.ToCanonical(woiop, &pk.Domain[0]).ToRegular(woiop)

	// TODO l, r, o before committing
	// Commit to l, r, o (blinded)
	if err := commitToLRO(wliop.P.Coefficients, wriop.P.Coefficients, woiop.P.Coefficients, proof, pk.Vk.KZGSRS); err != nil {
		return nil, err
	}

	// The first challenge is derived using the public data: the commitments to the permutation,
	// the coefficients of the circuit, and the public inputs.
	// derive gamma from the Comm(blinded cl), Comm(blinded cr), Comm(blinded co)
	if err := bindPublicData(&fs, "gamma", *pk.Vk, fullWitness[:len(spr.Public)]); err != nil {
		return nil, err
	}
	gamma, err := deriveRandomness(&fs, "gamma", &proof.LRO[0], &proof.LRO[1], &proof.LRO[2])
	if err != nil {
		return nil, err
	}

	// Fiat Shamir this
	bbeta, err := fs.ComputeChallenge("beta")
	if err != nil {
		return nil, err
	}
	var beta fr.Element
	beta.SetBytes(bbeta)

	// compute the copy constraint's ratio
	// We copy liop, riop, oiop because they are fft'ed in the process.
	// We could have not copied them at the cost of doing one more bit reverse
	// per poly...
	ziop, err := iop.BuildRatioCopyConstraint(
		[]*iop.Polynomial{liop.Copy(), riop.Copy(), oiop.Copy()},
		pk.Permutation,
		beta,
		gamma,
		iop.Form{Basis: iop.Canonical, Layout: iop.Regular},
		&pk.Domain[0],
	)
	if err != nil {
		return proof, err
	}

	// TODO blind z here
	// commit to the blinded version of z
	proof.Z, err = kzg.Commit(ziop.Coefficients, pk.Vk.KZGSRS, runtime.NumCPU()*2)
	if err != nil {
		return proof, err
	}

	// derive alpha from the Comm(l), Comm(r), Comm(o), Com(Z)
	alpha, err := deriveRandomness(&fs, "alpha", &proof.Z)
	if err != nil {
		return proof, err
	}

	// compute qk in canonical basis, completed with the public inputs
	qkCompletedCanonical := make([]fr.Element, pk.Domain[0].Cardinality)
	copy(qkCompletedCanonical, fullWitness[:len(spr.Public)])
	copy(qkCompletedCanonical[len(spr.Public):], pk.LQk[len(spr.Public):])
	pk.Domain[0].FFTInverse(qkCompletedCanonical, fft.DIF)
	fft.BitReverse(qkCompletedCanonical)

	// evaluate qlL+qrR+qmLR+qoO+qK (l,r,o=x₅,x₆,x₇)
	var constraintsCapture iop.MultivariatePolynomial
	var one fr.Element
	one.SetOne()
	constraintsCapture.AddMonomial(one, []int{1, 0, 0, 0, 0, 1, 0, 0})
	constraintsCapture.AddMonomial(one, []int{0, 1, 0, 0, 0, 0, 1, 0})
	constraintsCapture.AddMonomial(one, []int{0, 0, 1, 0, 0, 1, 1, 0})
	constraintsCapture.AddMonomial(one, []int{0, 0, 0, 1, 0, 0, 0, 1})
	constraintsCapture.AddMonomial(one, []int{0, 0, 0, 0, 1, 0, 0, 0})

	// l, r, o are blinded here
	wliop.ToLagrangeCoset(wliop, &pk.Domain[1])
	wriop.ToLagrangeCoset(wriop, &pk.Domain[1])
	woiop.ToLagrangeCoset(woiop, &pk.Domain[1])
	canReg := iop.Form{Basis: iop.Canonical, Layout: iop.Regular}
	wqliop := iop.NewPolynomial(pk.Ql, canReg).WrapMe(0)
	wqriop := iop.NewPolynomial(pk.Qr, canReg).WrapMe(0)
	wqmiop := iop.NewPolynomial(pk.Qm, canReg).WrapMe(0)
	wqoiop := iop.NewPolynomial(pk.Qo, canReg).WrapMe(0)
	wqkiop := iop.NewPolynomial(qkCompletedCanonical, canReg).WrapMe(0)
	wqliop.ToLagrangeCoset(wqliop, &pk.Domain[1])
	wqriop.ToLagrangeCoset(wqriop, &pk.Domain[1])
	wqmiop.ToLagrangeCoset(wqmiop, &pk.Domain[1])
	wqoiop.ToLagrangeCoset(wqoiop, &pk.Domain[1])
	wqkiop.ToLagrangeCoset(wqkiop, &pk.Domain[1])

	constraints, err := constraintsCapture.EvaluatePolynomials(
		[]iop.WrappedPolynomial{*wqliop, *wqriop, *wqmiop, *wqoiop, *wqkiop, *wliop, *wriop, *woiop},
	)
	if err != nil {
		return proof, err
	} // -> CORRECT

	// constraints ordering, l, r, o, z are blinded here
	var subOrderingCapture [3]iop.MultivariatePolynomial
	var ubeta, uubeta fr.Element
	ubeta.Mul(&beta, &pk.Domain[0].FrMultiplicativeGen)
	uubeta.Mul(&ubeta, &pk.Domain[0].FrMultiplicativeGen)
	subOrderingCapture[0].AddMonomial(one, []int{1, 0})
	subOrderingCapture[0].AddMonomial(beta, []int{0, 1})
	subOrderingCapture[0].C.Set(&gamma)
	subOrderingCapture[1].AddMonomial(one, []int{1, 0})
	subOrderingCapture[1].AddMonomial(ubeta, []int{0, 1})
	subOrderingCapture[1].C.Set(&gamma)
	subOrderingCapture[2].AddMonomial(one, []int{1, 0})
	subOrderingCapture[2].AddMonomial(uubeta, []int{0, 1})
	subOrderingCapture[2].C.Set(&gamma)

	// ql+β*x+γ
	id := make([]fr.Element, pk.Domain[1].Cardinality)
	id[1].SetOne()
	widiop := iop.NewPolynomial(id, canReg).WrapMe(0)
	widiop.ToLagrangeCoset(widiop, &pk.Domain[1])
	a, err := subOrderingCapture[0].EvaluatePolynomials([]iop.WrappedPolynomial{*wliop, *widiop})
	if err != nil {
		return proof, err
	}
	wa := a.WrapMe(0) // -> CORRECT

	// qr+β*ν*x+γ
	b, err := subOrderingCapture[1].EvaluatePolynomials([]iop.WrappedPolynomial{*wriop, *widiop})
	if err != nil {
		return proof, err
	}
	wb := b.WrapMe(0) // -> CORRECT

	// qo+β*ν²*x+γ
	c, err := subOrderingCapture[2].EvaluatePolynomials([]iop.WrappedPolynomial{*woiop, *widiop})
	if err != nil {
		return proof, err
	}
	wc := c.WrapMe(0) // -> CORRECT

	// ql+β*σ₁+γ
	ws1 := iop.NewPolynomial(pk.S1Canonical, canReg).WrapMe(0)
	ws1.ToCanonical(ws1, &pk.Domain[0]).ToRegular(ws1).ToLagrangeCoset(ws1, &pk.Domain[1])
	u, err := subOrderingCapture[0].EvaluatePolynomials([]iop.WrappedPolynomial{*wliop, *ws1})
	if err != nil {
		return proof, err
	}
	wu := u.WrapMe(0) // -> CORRECT

	// qr+β*σ₂+γ
	ws2 := iop.NewPolynomial(pk.S2Canonical, canReg).WrapMe(0)
	ws2.ToCanonical(ws2, &pk.Domain[0]).ToRegular(ws2).ToLagrangeCoset(ws2, &pk.Domain[1])
	v, err := subOrderingCapture[0].EvaluatePolynomials([]iop.WrappedPolynomial{*wriop, *ws2})
	if err != nil {
		return proof, err
	}
	wv := v.WrapMe(0) // -> CORRECT

	// qo+β*σ₃+γ
	ws3 := iop.NewPolynomial(pk.S3Canonical, canReg).WrapMe(0)
	ws3.ToCanonical(ws3, &pk.Domain[0]).ToRegular(ws3).ToLagrangeCoset(ws3, &pk.Domain[1])
	w, err := subOrderingCapture[0].EvaluatePolynomials([]iop.WrappedPolynomial{*woiop, *ws3})
	if err != nil {
		return proof, err
	}
	ww := w.WrapMe(0) // -> CORRECT

	// Z(ωX)(ql+β*σ₁+γ)(ql+β*σ₂+γ)(ql+β*σ₃+γ)-
	// Z(ql+βX+γ)(ql+β*νX+γ)(ql+β*ν²X+γ)
	var orderingCapture iop.MultivariatePolynomial
	var minusOne fr.Element
	wziop := ziop.WrapMe(0)
	wsziop := ziop.WrapMe(1)
	wsziop.ToCanonical(wsziop, &pk.Domain[0]).ToRegular(wsziop).ToLagrangeCoset(wsziop, &pk.Domain[1])
	minusOne.Neg(&one)
	orderingCapture.AddMonomial(one, []int{1, 1, 1, 1, 0, 0, 0, 0})
	orderingCapture.AddMonomial(minusOne, []int{0, 0, 0, 0, 1, 1, 1, 1})
	ordering, err := orderingCapture.EvaluatePolynomials(
		[]iop.WrappedPolynomial{*wsziop, *wu, *wv, *ww, *wziop, *wa, *wb, *wc})
	if err != nil {
		return proof, err
	}

	// L₀(z-1), z is blinded
	lone := make([]fr.Element, pk.Domain[0].Cardinality)
	lone[0].SetOne()
	loneiop := iop.NewPolynomial(lone, lagReg)
	wloneiop := loneiop.ToCanonical(loneiop, &pk.Domain[0]).
		ToRegular(loneiop).
		ToLagrangeCoset(loneiop, &pk.Domain[1]).
		WrapMe(0)
	var startsAtOneCapture iop.MultivariatePolynomial
	startsAtOneCapture.AddMonomial(one, []int{1, 1})
	startsAtOneCapture.AddMonomial(minusOne, []int{0, 1})
	startsAtOne, err := startsAtOneCapture.EvaluatePolynomials(
		[]iop.WrappedPolynomial{*wziop, *wloneiop},
	)
	if err != nil {
		return proof, err
	}

	// bundle everything up using α
	var plonkCapture iop.MultivariatePolynomial
	var alphaSquared fr.Element
	alphaSquared.Square(&alpha)
	plonkCapture.AddMonomial(one, []int{1, 0, 0})
	plonkCapture.AddMonomial(alpha, []int{0, 1, 0})
	plonkCapture.AddMonomial(alphaSquared, []int{0, 0, 1})

	wconstraints := constraints.WrapMe(0)
	wordering := ordering.WrapMe(0)
	wstartsAtOne := startsAtOne.WrapMe(0)

	h, err := iop.ComputeQuotient(
		[]iop.WrappedPolynomial{*wconstraints, *wordering, *wstartsAtOne},
		plonkCapture,
		[2]*fft.Domain{&pk.Domain[0], &pk.Domain[1]})
	if err != nil {
		return proof, err
	}

	// compute kzg commitments of h1, h2 and h3
	if err := commitToQuotient(
		h.Coefficients[:pk.Domain[0].Cardinality+2],
		h.Coefficients[pk.Domain[0].Cardinality+2:2*(pk.Domain[0].Cardinality+2)],
		h.Coefficients[2*(pk.Domain[0].Cardinality+2):3*(pk.Domain[0].Cardinality+2)],
		proof, pk.Vk.KZGSRS); err != nil {
		return nil, err
	}

	// derive zeta
	zeta, err := deriveRandomness(&fs, "zeta", &proof.H[0], &proof.H[1], &proof.H[2])
	if err != nil {
		return nil, err
	}

	// compute evaluations of (blinded version of) l, r, o, z at zeta
	wliop.ToCanonical(wliop, &pk.Domain[1]).ToRegular(wliop)
	wriop.ToCanonical(wriop, &pk.Domain[1]).ToRegular(wriop)
	woiop.ToCanonical(woiop, &pk.Domain[1]).ToRegular(woiop)

	// var blzeta, brzeta, bozeta fr.Element
	blzeta := wliop.Evaluate(zeta)
	brzeta := wriop.Evaluate(zeta)
	bozeta := woiop.Evaluate(zeta)
	// -> CORRECT

	// open blinded Z at zeta*z
	wziop.ToCanonical(wziop, &pk.Domain[1]).ToRegular(wziop)
	var zetaShifted fr.Element
	zetaShifted.Mul(&zeta, &pk.Vk.Generator)
	proof.ZShiftedOpening, err = kzg.Open(
		wziop.P.Coefficients[:pk.Domain[0].Cardinality],
		zetaShifted,
		pk.Vk.KZGSRS,
	)
	if err != nil {
		return nil, err
	}

	// blinded z evaluated at u*zeta
	bzuzeta := proof.ZShiftedOpening.ClaimedValue

	var (
		linearizedPolynomialCanonical []fr.Element
		linearizedPolynomialDigest    curve.G1Affine
		errLPoly                      error
	)

	// compute the linearization polynomial r at zeta
	// (goal: save committing separately to z, ql, qr, qm, qo, k
	linearizedPolynomialCanonical = computeLinearizedPolynomial(
		blzeta,
		brzeta,
		bozeta,
		alpha,
		beta,
		gamma,
		zeta,
		bzuzeta,
		wziop.P.Coefficients[:pk.Domain[0].Cardinality+2],
		pk,
	)

	// TODO this commitment is only necessary to derive the challenge, we should
	// be able to avoid doing it and get the challenge in another way
	linearizedPolynomialDigest, errLPoly = kzg.Commit(linearizedPolynomialCanonical, pk.Vk.KZGSRS)

	// foldedHDigest = Comm(h1) + ζᵐ⁺²*Comm(h2) + ζ²⁽ᵐ⁺²⁾*Comm(h3)
	var bZetaPowerm, bSize big.Int
	bSize.SetUint64(pk.Domain[0].Cardinality + 2) // +2 because of the masking (h of degree 3(n+2)-1)
	var zetaPowerm fr.Element
	zetaPowerm.Exp(zeta, &bSize)
	zetaPowerm.BigInt(&bZetaPowerm)
	foldedHDigest := proof.H[2]
	foldedHDigest.ScalarMultiplication(&foldedHDigest, &bZetaPowerm)
	foldedHDigest.Add(&foldedHDigest, &proof.H[1])                   // ζᵐ⁺²*Comm(h3)
	foldedHDigest.ScalarMultiplication(&foldedHDigest, &bZetaPowerm) // ζ²⁽ᵐ⁺²⁾*Comm(h3) + ζᵐ⁺²*Comm(h2)
	foldedHDigest.Add(&foldedHDigest, &proof.H[0])                   // ζ²⁽ᵐ⁺²⁾*Comm(h3) + ζᵐ⁺²*Comm(h2) + Comm(h1)

	// foldedH = h1 + ζ*h2 + ζ²*h3
	foldedH := h.Coefficients[2*(pk.Domain[0].Cardinality+2) : 3*(pk.Domain[0].Cardinality+2)]
	h2 := h.Coefficients[pk.Domain[0].Cardinality+2 : 2*(pk.Domain[0].Cardinality+2)]
	h1 := h.Coefficients[:pk.Domain[0].Cardinality+2]
	utils.Parallelize(len(foldedH), func(start, end int) {
		for i := start; i < end; i++ {
			foldedH[i].Mul(&foldedH[i], &zetaPowerm) // ζᵐ⁺²*h3
			foldedH[i].Add(&foldedH[i], &h2[i])      // ζ^{m+2)*h3+h2
			foldedH[i].Mul(&foldedH[i], &zetaPowerm) // ζ²⁽ᵐ⁺²⁾*h3+h2*ζᵐ⁺²
			foldedH[i].Add(&foldedH[i], &h1[i])      // ζ^{2(m+2)*h3+ζᵐ⁺²*h2 + h1
		}
	})

	if errLPoly != nil {
		return nil, errLPoly
	}

	// Batch open the first list of polynomials
	proof.BatchedProof, err = kzg.BatchOpenSinglePoint(
		[][]fr.Element{
			foldedH,
			linearizedPolynomialCanonical,
			wliop.P.Coefficients[:wliop.Size],
			wriop.P.Coefficients[:wriop.Size],
			woiop.P.Coefficients[:woiop.Size],
			pk.S1Canonical,
			pk.S2Canonical,
		},
		[]kzg.Digest{
			foldedHDigest,
			linearizedPolynomialDigest,
			proof.LRO[0],
			proof.LRO[1],
			proof.LRO[2],
			pk.Vk.S[0],
			pk.Vk.S[1],
		},
		zeta,
		hFunc,
		pk.Vk.KZGSRS,
	)

	log.Debug().Dur("took", time.Since(start)).Msg("prover done")

	if err != nil {
		return nil, err
	}

	return proof, nil

}

// fills proof.LRO with kzg commits of bcl, bcr and bco
func commitToLRO(bcl, bcr, bco []fr.Element, proof *Proof, srs *kzg.SRS) error {
	n := runtime.NumCPU() / 2
	var err0, err1, err2 error
	chCommit0 := make(chan struct{}, 1)
	chCommit1 := make(chan struct{}, 1)
	go func() {
		proof.LRO[0], err0 = kzg.Commit(bcl, srs, n)
		close(chCommit0)
	}()
	go func() {
		proof.LRO[1], err1 = kzg.Commit(bcr, srs, n)
		close(chCommit1)
	}()
	if proof.LRO[2], err2 = kzg.Commit(bco, srs, n); err2 != nil {
		return err2
	}
	<-chCommit0
	<-chCommit1

	if err0 != nil {
		return err0
	}

	return err1
}

func commitToQuotient(h1, h2, h3 []fr.Element, proof *Proof, srs *kzg.SRS) error {
	n := runtime.NumCPU() / 2
	var err0, err1, err2 error
	chCommit0 := make(chan struct{}, 1)
	chCommit1 := make(chan struct{}, 1)
	go func() {
		proof.H[0], err0 = kzg.Commit(h1, srs, n)
		close(chCommit0)
	}()
	go func() {
		proof.H[1], err1 = kzg.Commit(h2, srs, n)
		close(chCommit1)
	}()
	if proof.H[2], err2 = kzg.Commit(h3, srs, n); err2 != nil {
		return err2
	}
	<-chCommit0
	<-chCommit1

	if err0 != nil {
		return err0
	}

	return err1
}

// evaluateLROSmallDomain extracts the solution l, r, o, and returns it in lagrange form.
// solution = [ public | secret | internal ]
func evaluateLROSmallDomain(spr *cs.SparseR1CS, pk *ProvingKey, solution []fr.Element) ([]fr.Element, []fr.Element, []fr.Element) {

	s := int(pk.Domain[0].Cardinality)

	var l, r, o []fr.Element
	l = make([]fr.Element, s)
	r = make([]fr.Element, s)
	o = make([]fr.Element, s)
	s0 := solution[0]

	for i := 0; i < len(spr.Public); i++ { // placeholders
		l[i] = solution[i]
		r[i] = s0
		o[i] = s0
	}
	offset := len(spr.Public)
	for i := 0; i < len(spr.Constraints); i++ { // constraints
		l[offset+i] = solution[spr.Constraints[i].L.WireID()]
		r[offset+i] = solution[spr.Constraints[i].R.WireID()]
		o[offset+i] = solution[spr.Constraints[i].O.WireID()]
	}
	offset += len(spr.Constraints)

	for i := 0; i < s-offset; i++ { // offset to reach 2**n constraints (where the id of l,r,o is 0, so we assign solution[0])
		l[offset+i] = s0
		r[offset+i] = s0
		o[offset+i] = s0
	}

	return l, r, o

}

// computeLinearizedPolynomial computes the linearized polynomial in canonical basis.
// The purpose is to commit and open all in one ql, qr, qm, qo, qk.
// * lZeta, rZeta, oZeta are the evaluation of l, r, o at zeta
// * z is the permutation polynomial, zu is Z(μX), the shifted version of Z
// * pk is the proving key: the linearized polynomial is a linear combination of ql, qr, qm, qo, qk.
//
// The Linearized polynomial is:
//
// α²*L₁(ζ)*Z(X)
// + α*( (l(ζ)+β*s1(ζ)+γ)*(r(ζ)+β*s2(ζ)+γ)*Z(μζ)*s3(X) - Z(X)*(l(ζ)+β*id1(ζ)+γ)*(r(ζ)+β*id2(ζ)+γ)*(o(ζ)+β*id3(ζ)+γ))
// + l(ζ)*Ql(X) + l(ζ)r(ζ)*Qm(X) + r(ζ)*Qr(X) + o(ζ)*Qo(X) + Qk(X)
func computeLinearizedPolynomial(lZeta, rZeta, oZeta, alpha, beta, gamma, zeta, zu fr.Element, blindedZCanonical []fr.Element, pk *ProvingKey) []fr.Element {

	// first part: individual constraints
	var rl fr.Element
	rl.Mul(&rZeta, &lZeta)

	// second part:
	// Z(μζ)(l(ζ)+β*s1(ζ)+γ)*(r(ζ)+β*s2(ζ)+γ)*β*s3(X)-Z(X)(l(ζ)+β*id1(ζ)+γ)*(r(ζ)+β*id2(ζ)+γ)*(o(ζ)+β*id3(ζ)+γ)
	var s1, s2 fr.Element
	chS1 := make(chan struct{}, 1)
	go func() {
		ps1 := iop.NewPolynomial(pk.S1Canonical, iop.Form{Basis: iop.Canonical, Layout: iop.Regular})
		s1 = ps1.Evaluate(zeta)                              // s1(ζ)
		s1.Mul(&s1, &beta).Add(&s1, &lZeta).Add(&s1, &gamma) // (l(ζ)+β*s1(ζ)+γ)
		close(chS1)
	}()
	ps2 := iop.NewPolynomial(pk.S2Canonical, iop.Form{Basis: iop.Canonical, Layout: iop.Regular})
	tmp := ps2.Evaluate(zeta)                                // s2(ζ)
	tmp.Mul(&tmp, &beta).Add(&tmp, &rZeta).Add(&tmp, &gamma) // (r(ζ)+β*s2(ζ)+γ)
	<-chS1
	s1.Mul(&s1, &tmp).Mul(&s1, &zu).Mul(&s1, &beta) // (l(ζ)+β*s1(β)+γ)*(r(ζ)+β*s2(β)+γ)*β*Z(μζ)

	var uzeta, uuzeta fr.Element
	uzeta.Mul(&zeta, &pk.Vk.CosetShift)
	uuzeta.Mul(&uzeta, &pk.Vk.CosetShift)

	s2.Mul(&beta, &zeta).Add(&s2, &lZeta).Add(&s2, &gamma)      // (l(ζ)+β*ζ+γ)
	tmp.Mul(&beta, &uzeta).Add(&tmp, &rZeta).Add(&tmp, &gamma)  // (r(ζ)+β*u*ζ+γ)
	s2.Mul(&s2, &tmp)                                           // (l(ζ)+β*ζ+γ)*(r(ζ)+β*u*ζ+γ)
	tmp.Mul(&beta, &uuzeta).Add(&tmp, &oZeta).Add(&tmp, &gamma) // (o(ζ)+β*u²*ζ+γ)
	s2.Mul(&s2, &tmp)                                           // (l(ζ)+β*ζ+γ)*(r(ζ)+β*u*ζ+γ)*(o(ζ)+β*u²*ζ+γ)
	s2.Neg(&s2)                                                 // -(l(ζ)+β*ζ+γ)*(r(ζ)+β*u*ζ+γ)*(o(ζ)+β*u²*ζ+γ)

	// third part L₁(ζ)*α²*Z
	var lagrangeZeta, one, den, frNbElmt fr.Element
	one.SetOne()
	nbElmt := int64(pk.Domain[0].Cardinality)
	lagrangeZeta.Set(&zeta).
		Exp(lagrangeZeta, big.NewInt(nbElmt)).
		Sub(&lagrangeZeta, &one)
	frNbElmt.SetUint64(uint64(nbElmt))
	den.Sub(&zeta, &one).
		Inverse(&den)
	lagrangeZeta.Mul(&lagrangeZeta, &den). // L₁ = (ζⁿ⁻¹)/(ζ-1)
						Mul(&lagrangeZeta, &alpha).
						Mul(&lagrangeZeta, &alpha).
						Mul(&lagrangeZeta, &pk.Domain[0].CardinalityInv) // (1/n)*α²*L₁(ζ)

	linPol := make([]fr.Element, len(blindedZCanonical))
	copy(linPol, blindedZCanonical)

	utils.Parallelize(len(linPol), func(start, end int) {

		var t0, t1 fr.Element

		for i := start; i < end; i++ {

			linPol[i].Mul(&linPol[i], &s2) // -Z(X)*(l(ζ)+β*ζ+γ)*(r(ζ)+β*u*ζ+γ)*(o(ζ)+β*u²*ζ+γ)

			if i < len(pk.S3Canonical) {

				t0.Mul(&pk.S3Canonical[i], &s1) // (l(ζ)+β*s1(ζ)+γ)*(r(ζ)+β*s2(ζ)+γ)*Z(μζ)*β*s3(X)

				linPol[i].Add(&linPol[i], &t0)
			}

			linPol[i].Mul(&linPol[i], &alpha) // α*( (l(ζ)+β*s1(ζ)+γ)*(r(ζ)+β*s2(ζ)+γ)*Z(μζ)*s3(X) - Z(X)*(l(ζ)+β*ζ+γ)*(r(ζ)+β*u*ζ+γ)*(o(ζ)+β*u²*ζ+γ))

			if i < len(pk.Qm) {

				t1.Mul(&pk.Qm[i], &rl) // linPol = linPol + l(ζ)r(ζ)*Qm(X)
				t0.Mul(&pk.Ql[i], &lZeta)
				t0.Add(&t0, &t1)
				linPol[i].Add(&linPol[i], &t0) // linPol = linPol + l(ζ)*Ql(X)

				t0.Mul(&pk.Qr[i], &rZeta)
				linPol[i].Add(&linPol[i], &t0) // linPol = linPol + r(ζ)*Qr(X)

				t0.Mul(&pk.Qo[i], &oZeta).Add(&t0, &pk.CQk[i])
				linPol[i].Add(&linPol[i], &t0) // linPol = linPol + o(ζ)*Qo(X) + Qk(X)
			}

			t0.Mul(&blindedZCanonical[i], &lagrangeZeta)
			linPol[i].Add(&linPol[i], &t0) // finish the computation
		}
	})
	return linPol
}
