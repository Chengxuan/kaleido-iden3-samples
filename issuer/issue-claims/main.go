// Copyright © 2022 Kaleido, Inc.
//
// SPDX-License-Identifier: Apache-2.0
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

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"

	"github.com/iden3/go-circuits"
	core "github.com/iden3/go-iden3-core"
	"github.com/iden3/go-iden3-crypto/babyjub"
	"github.com/iden3/go-iden3-crypto/keccak256"
	"github.com/iden3/go-iden3-crypto/poseidon"
	merkletree "github.com/iden3/go-merkletree-sql"
	sqlstorage "github.com/iden3/go-merkletree-sql/db/sql"
	_ "github.com/mattn/go-sqlite3"
)

type ClaimInputs struct {
	IssuerClaim                     *core.Claim      `json:"issuerClaim"`
	IssuerClaimNonRevClaimsTreeRoot *merkletree.Hash `json:"issuerClaimNonRevClaimsTreeRoot"`
	IssuerClaimNonRevRevTreeRoot    *merkletree.Hash `json:"issuerClaimNonRevRevTreeRoot"`
	IssuerClaimNonRevRootsTreeRoot  *merkletree.Hash `json:"issuerClaimNonRevRootsTreeRoot"`
	IssuerClaimNonRevState          *merkletree.Hash `json:"issuerClaimNonRevState"`
	IssuerClaimNonRevMtp            []string         `json:"issuerClaimNonRevMtp"`
	IssuerClaimNonRevMtpAuxHi       *merkletree.Hash `json:"issuerClaimNonRevMtpAuxHi"`
	IssuerClaimNonRevMtpAuxHv       *merkletree.Hash `json:"issuerClaimNonRevMtpAuxHv"`
	IssuerClaimNonRevMtpNoAux       string           `json:"issuerClaimNonRevMtpNoAux"`
	ClaimSchema                     string           `json:"claimSchema"`
	IssuerClaimSignatureR8X         string           `json:"issuerClaimSignatureR8x"`
	IssuerClaimSignatureR8Y         string           `json:"issuerClaimSignatureR8y"`
	IssuerClaimSignatureS           string           `json:"issuerClaimSignatureS"`
}

const SqlStorageSchema = `CREATE TABLE IF NOT EXISTS mt_nodes (
	mt_id BIGINT,
	key BYTEA,
	type SMALLINT NOT NULL,
	child_l BYTEA,
	child_r BYTEA,
	entry BYTEA,
	created_at BIGINT,
	deleted_at BIGINT,
	PRIMARY KEY(mt_id, key)
);

CREATE TABLE IF NOT EXISTS mt_roots (
	mt_id BIGINT PRIMARY KEY,
	key BYTEA,
	created_at BIGINT,
	deleted_at BIGINT
);`

func main() {
	homedir, _ := os.UserHomeDir()
	dbPath := filepath.Join(homedir, "iden3/issuer.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		fmt.Printf("Failed to open sqlite db: %s\n", err)
		os.Exit(1)
	}
	initMerkleTreeDB(db)

	switch os.Args[1] {
	case "init":
		generateIdentity(db)
	case "claim":
		issueClaim(db)
	default:
		fmt.Println(`Usage:
			init  - initialize a new issuer identity
			claim - issues a claim for a holder
		`)
		os.Exit(1)
	}
}

func initMerkleTreeDB(db *sql.DB) error {
	if _, err := db.Exec(SqlStorageSchema); err != nil {
		return err
	}
	return nil
}

type sqlDB struct {
	db *sql.DB
}

func (s *sqlDB) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return s.db.ExecContext(ctx, query, args...)
}

func (s *sqlDB) GetContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
	result, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if result.Next() {
		return result.Scan(dest)
	}
	return nil
}

func (s *sqlDB) SelectContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
	return fmt.Errorf("not implemented")
}

func generateIdentity(db *sql.DB) {
	fmt.Println("Generating new signing key from the \"babyjubjub\" curve")
	privKey := babyjub.NewRandPrivKey()
	pubKey := privKey.Public()
	fmt.Printf("-> Public key: %s\n\n", pubKey)

	ctx := context.Background()
	storage := sqlstorage.NewSqlStorage(&sqlDB{db: db}, 1)

	// An iden3 state is made up of 3 parts:
	// - a claims tree. This is a sparse merkle tree where each claim is uniquely identified with a key
	// - a revocation tree. This captures whether a claim, identified by its revocation nonce, has been revoked
	// - a roots tree. This captures the historical progression of the merkle tree root of the claims tree
	//
	// To create a genesis state:
	// - issue an auth claim based on the public key and revocation nounce, this will determine the identity's ID
	// - add the auth claim to the claim tree
	// - add the claim tree root at this point in time to the roots tree
	fmt.Println("Generating genesis state for the issuer")
	fmt.Println("-> Create the empty claims merkle tree")
	claimsTree, err := merkletree.NewMerkleTree(ctx, storage, 32)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Println("-> Create the empty revocations merkle tree")
	revocationsTree, _ := merkletree.NewMerkleTree(ctx, storage, 32)
	fmt.Println("-> Create the empty roots merkle tree")
	rootsTree, _ := merkletree.NewMerkleTree(ctx, storage, 32)

	// A schema is registered using its hash. The hash is used to coordinate the validation by offline processes.
	// There is no schema validation by the protocol.
	fmt.Println("-> Issue the authentication claim for the issuer's identity")
	authSchemaHash, _ := getAuthClaimSchemaHash()
	revNonce := uint64(1)
	// An auth claim includes the X and Y curve coordinates of the public key, along with the revocation nonce
	authClaim, _ := core.NewClaim(authSchemaHash, core.WithIndexDataInts(pubKey.X, pubKey.Y), core.WithRevocationNonce(revNonce))
	encodedAuthClaim, _ := json.Marshal(authClaim)
	fmt.Printf("   -> Issued auth claim: encoded=%s\n", encodedAuthClaim)

	fmt.Println("   -> Add the new auth claim to the claims tree")
	hIndex, hValue, _ := authClaim.HiHv()
	claimsTree.Add(ctx, hIndex, hValue)

	// print the genesis state
	genesisState, _ := merkletree.HashElems(claimsTree.Root().BigInt(), revocationsTree.Root().BigInt(), rootsTree.Root().BigInt())
	fmt.Printf("-> Genesis State: %s\n", genesisState.BigInt())

	// print the ID
	id, _ := core.IdGenesisFromIdenState(core.TypeDefault, genesisState.BigInt())
	fmt.Printf("-> ID of the issuer identity: %s\n\n", id)

	// construct the genesis state snapshot, to be used as input to the ZKP for the state transition
	fmt.Println("Construct the state snapshot (later as input to the ZK proof generation)")
	fmt.Println("-> Generate a merkle proof of the inclusion of the auth claim in the claims tree")
	authMTProof, _, _ := claimsTree.GenerateProof(ctx, hIndex, claimsTree.Root())
	fmt.Printf("-> Generate a merkle proof of the exclusion of the revocation nonce in the revocation tree\n\n")
	authNonRevMTProof, _, _ := revocationsTree.GenerateProof(ctx, new(big.Int).SetInt64(int64(revNonce)), revocationsTree.Root())
	genesisTreeState := circuits.TreeState{
		State:          genesisState,
		ClaimsRoot:     claimsTree.Root(),
		RevocationRoot: revocationsTree.Root(),
		RootOfRoots:    rootsTree.Root(),
	}

	// before updating the claims tree, add the claims tree root at this point to the roots tree
	fmt.Printf("Add the current claim tree root to the roots tree\n")
	rootsTree.Add(ctx, claimsTree.Root().BigInt(), big.NewInt(0))

	content := make([]byte, 32)
	src := privKey[:]
	copy(content[:], src)
	os.WriteFile(getPrivateKeyPath(), []byte(content), 0644)
	persistNewState(claimsTree, revocationsTree, rootsTree, genesisTreeState, privKey, id, authClaim, authMTProof, authNonRevMTProof)
}

func issueClaim(db *sql.DB) {
	//
	// Before issuing any claims, we need to first load the ID of the holder
	//
	fmt.Println("Load issuer identity")

	homedir, _ := os.UserHomeDir()
	issuerIdBytes, _ := os.ReadFile(filepath.Join(homedir, "iden3_issuer_stateTransition_inputs.json"))
	var issuerIdInputs map[string]interface{}
	err := json.Unmarshal(issuerIdBytes, &issuerIdInputs)
	if err != nil {
		fmt.Printf("-> Failed to read holder ID file: %s\n", err)
		os.Exit(1)
	}
	issuerIdStr := issuerIdInputs["userID"].(string)
	issuerIdBigInt := &big.Int{}
	issuerIdBigInt.SetString(issuerIdStr, 10)
	issuerId, err := core.IDFromInt(issuerIdBigInt)
	fmt.Println("-> Issuer identity: ", issuerId.String())
	if err != nil {
		fmt.Printf("-> Failed to load issuer ID from string: %s\n", err)
		os.Exit(1)
	}

	issueCmd := flag.NewFlagSet("claim", flag.ExitOnError)
	holderIdStr := issueCmd.String("holderId", "", "base58-encoded ID of the holder")
	revNonce := issueCmd.Uint64("nonce", 2, "Revocation nonce for the new claim")
	issueCmd.Parse(os.Args[2:])
	if *holderIdStr == "" {
		fmt.Println("Must specify a base58-encoded ID for the holder with a --holderId parameter")
		os.Exit(1)
	}
	if *revNonce == 0 {
		fmt.Println("Must specify a revocation nonce for the new claim with a --nonce parameter")
		os.Exit(1)
	}
	holderId, err := core.IDFromString(*holderIdStr)
	if err != nil {
		fmt.Println("Failed to use the provided Id: ", err)
		os.Exit(1)
	}
	fmt.Println("Using holder identity: ", *holderIdStr)
	fmt.Println("Using revocation nonce for the new claim: ", revNonce)

	ctx := context.Background()
	storage := sqlstorage.NewSqlStorage(&sqlDB{db: db}, 1)
	fmt.Println("Load the issuer claims merkle tree")
	claimsTree, _ := merkletree.NewMerkleTree(ctx, storage, 32)
	fmt.Println("Load the revocations merkle tree")
	revocationsTree, _ := merkletree.NewMerkleTree(ctx, storage, 32)
	fmt.Printf("Load the roots merkle tree\n")
	rootsTree, _ := merkletree.NewMerkleTree(ctx, storage, 32)

	fmt.Println("Issue the KYC age claim")
	// Load the schema for the KYC claims
	schemaBytes, _ := os.ReadFile("./schemas/test.json-ld")
	var sHash core.SchemaHash

	// issue the age claim
	h := keccak256.Hash(schemaBytes, []byte("KYCAgeCredential"))
	copy(sHash[:], h[len(h)-16:])
	sHashText, _ := sHash.MarshalText()
	ageSchemaHash := string(sHashText)
	fmt.Println("-> Schema hash for 'KYCAgeCredential':", ageSchemaHash)

	kycAgeSchema, _ := core.NewSchemaHashFromHex(ageSchemaHash)
	age := big.NewInt(25)
	ageClaim, _ := core.NewClaim(kycAgeSchema, core.WithIndexID(holderId), core.WithIndexDataInts(age, nil), core.WithRevocationNonce(*revNonce))
	// kycClaim, err := core.NewClaim(kycSchema, core.WithIndexDataBytes([]byte("Lionel Messi"), []byte("ACCOUNT1234567890")), core.WithValueDataBytes([]byte("US"), []byte("295816c03b74e65ac34e5c6dda3c75")))
	encoded, _ := json.Marshal(ageClaim)
	fmt.Printf("-> Issued age claim: %s\n", encoded)

	// add the age claim to the claim tree
	fmt.Printf("-> Add the age claim to the claims tree\n\n")
	ageHashIndex, ageHashValue, _ := ageClaim.HiHv()
	claimsTree.Add(ctx, ageHashIndex, ageHashValue)

	keyBytes, err := os.ReadFile(getPrivateKeyPath())
	if err != nil {
		fmt.Println("Failed to load private key: ", err)
		os.Exit(1)
	}
	var key32Bytes [32]byte
	copy(key32Bytes[:], keyBytes)
	privKey := babyjub.PrivateKey(key32Bytes)
	// from the recovered private key, we can derive the public key and the auth claim
	pubKey := privKey.Public()
	authClaimRevNonce := uint64(1)
	authSchemaHash, _ := getAuthClaimSchemaHash()
	// An auth claim includes the X and Y curve coordinates of the public key, along with the revocation nonce
	authClaim, _ := core.NewClaim(authSchemaHash, core.WithIndexDataInts(pubKey.X, pubKey.Y), core.WithRevocationNonce(authClaimRevNonce))
	hIndex, _ := authClaim.HIndex()
	authMTProof, _, _ := claimsTree.GenerateProof(ctx, hIndex, claimsTree.Root())
	authNonRevMTProof, _, _ := revocationsTree.GenerateProof(ctx, new(big.Int).SetInt64(int64(authClaimRevNonce)), revocationsTree.Root())

	issuerState, _ := merkletree.HashElems(claimsTree.Root().BigInt(), revocationsTree.Root().BigInt(), rootsTree.Root().BigInt())
	issuerTreeState := circuits.TreeState{
		State:          issuerState,
		ClaimsRoot:     claimsTree.Root(),
		RevocationRoot: revocationsTree.Root(),
		RootOfRoots:    rootsTree.Root(),
	}

	persistInputsForAtomicQuerySig(ctx, ageClaim, *revNonce, claimsTree, revocationsTree, rootsTree, privKey, &issuerId, issuerTreeState, authClaim, authMTProof, authNonRevMTProof)
}

func persistNewState(claimsTree, revocationsTree, rootsTree *merkletree.MerkleTree, oldTreeState circuits.TreeState, privKey babyjub.PrivateKey, id *core.ID, authClaim *core.Claim, authMTProof, authNonRevMTProof *merkletree.Proof) {
	// construct the new identity state
	fmt.Println("Calculate the new state")
	newState, _ := merkletree.HashElems(claimsTree.Root().BigInt(), revocationsTree.Root().BigInt(), rootsTree.Root().BigInt())

	// hash the [old state + new state] to be signed later
	hashOldAndNewState, _ := poseidon.Hash([]*big.Int{oldTreeState.State.BigInt(), newState.BigInt()})
	// sign using the identity key
	signature := privKey.SignPoseidon(hashOldAndNewState)

	// construct the inputs to feed to the proof generation for the state transition
	fmt.Println("-> state transition from old to new")
	stateTransitionInputs := circuits.StateTransitionInputs{
		ID:                id,
		OldTreeState:      oldTreeState,
		NewState:          newState,
		IsOldStateGenesis: true,
		AuthClaim: circuits.Claim{
			Claim: authClaim,
			Proof: authMTProof,
			NonRevProof: &circuits.ClaimNonRevStatus{
				Proof: authNonRevMTProof,
			},
		},
		Signature: signature,
	}

	homedir, _ := os.UserHomeDir()
	inputBytes, _ := stateTransitionInputs.InputsMarshal()
	outputFile := filepath.Join(homedir, "iden3/issuer/stateTransition_inputs.json")
	os.WriteFile(outputFile, inputBytes, 0644)
	fmt.Printf("-> State transition input bytes written to the file: %s\n", outputFile)
}

func persistInputsForAtomicQuerySig(ctx context.Context, ageClaim *core.Claim, ageNonce uint64, claimTree, revocationTree, rootsTree *merkletree.MerkleTree, privKey babyjub.PrivateKey, issuerId *core.ID, genesisTreeState circuits.TreeState, authClaim *core.Claim, authMTProof *merkletree.Proof, authNonRevMTProof *merkletree.Proof) {
	// persists additional inputs used for generating zk proofs by the holder
	// these are candidates for sending to the holder wallet
	ageHashIndex, _, _ := ageClaim.HiHv()
	ageClaimMTProof, _, _ := claimTree.GenerateProof(ctx, ageHashIndex, claimTree.Root())
	stateAfterAddingAgeClaim, _ := merkletree.HashElems(claimTree.Root().BigInt(), revocationTree.Root().BigInt(), rootsTree.Root().BigInt())
	issuerStateAfterClaimAdd := circuits.TreeState{
		State:          stateAfterAddingAgeClaim,
		ClaimsRoot:     claimTree.Root(),
		RevocationRoot: revocationTree.Root(),
		RootOfRoots:    rootsTree.Root(),
	}
	proofNotRevoke, _, _ := revocationTree.GenerateProof(ctx, big.NewInt(int64(ageNonce)), revocationTree.Root())
	hashIndex, hashValue, err := claimsIndexValueHashes(*ageClaim)
	if err != nil {
		fmt.Printf("Failed to calculate claim hashes: %s\n", err)
		os.Exit(1)
	}
	commonHash, _ := merkletree.HashElems(hashIndex, hashValue)
	claimSignature := privKey.SignPoseidon(commonHash.BigInt())
	claimIssuerSignature := circuits.BJJSignatureProof{
		IssuerID:           issuerId,
		IssuerTreeState:    genesisTreeState,
		IssuerAuthClaimMTP: authMTProof,
		Signature:          claimSignature,
		IssuerAuthClaim:    authClaim,
		IssuerAuthNonRevProof: circuits.ClaimNonRevStatus{
			TreeState: genesisTreeState,
			Proof:     authNonRevMTProof,
		},
	}
	inputsUserClaim := circuits.Claim{
		Claim:     ageClaim,
		Proof:     ageClaimMTProof,
		TreeState: issuerStateAfterClaimAdd,
		NonRevProof: &circuits.ClaimNonRevStatus{
			TreeState: issuerStateAfterClaimAdd,
			Proof:     proofNotRevoke,
		},
		IssuerID:       issuerId,
		SignatureProof: claimIssuerSignature,
	}
	key, value, noAux := getNodeAuxValue(inputsUserClaim.NonRevProof.Proof.NodeAux)
	a := circuits.AtomicQuerySigInputs{}
	inputs := ClaimInputs{
		IssuerClaim:                     inputsUserClaim.Claim,
		IssuerClaimNonRevClaimsTreeRoot: inputsUserClaim.NonRevProof.TreeState.ClaimsRoot,
		IssuerClaimNonRevRevTreeRoot:    inputsUserClaim.NonRevProof.TreeState.RevocationRoot,
		IssuerClaimNonRevRootsTreeRoot:  inputsUserClaim.NonRevProof.TreeState.RootOfRoots,
		IssuerClaimNonRevState:          inputsUserClaim.NonRevProof.TreeState.State,
		IssuerClaimNonRevMtp:            circuits.PrepareSiblingsStr(inputsUserClaim.NonRevProof.Proof.AllSiblings(), a.GetMTLevel()),
		ClaimSchema:                     inputsUserClaim.Claim.GetSchemaHash().BigInt().String(),
		IssuerClaimNonRevMtpAuxHi:       key,
		IssuerClaimNonRevMtpAuxHv:       value,
		IssuerClaimNonRevMtpNoAux:       noAux,
		IssuerClaimSignatureR8X:         claimSignature.R8.X.String(),
		IssuerClaimSignatureR8Y:         claimSignature.R8.Y.String(),
		IssuerClaimSignatureS:           claimSignature.S.String(),
	}
	homedir, _ := os.UserHomeDir()
	inputBytes, _ := json.Marshal(inputs)
	outputFile := filepath.Join(homedir, "iden3_input_issuer_to_user.json")
	os.WriteFile(outputFile, inputBytes, 0644)
	fmt.Printf("-> Input bytes for issued user claim written to the file: %s\n", outputFile)
}

func getNodeAuxValue(a *merkletree.NodeAux) (*merkletree.Hash, *merkletree.Hash, string) {

	key := &merkletree.HashZero
	value := &merkletree.HashZero
	noAux := "1"

	if a != nil && a.Value != nil && a.Key != nil {
		key = a.Key
		value = a.Value
		noAux = "0"
	}

	return key, value, noAux
}

func claimsIndexValueHashes(c core.Claim) (*big.Int, *big.Int, error) {
	index, value := c.RawSlots()
	indexHash, err := poseidon.Hash(core.ElemBytesToInts(index[:]))
	if err != nil {
		return nil, nil, err
	}
	valueHash, err := poseidon.Hash(core.ElemBytesToInts(value[:]))
	return indexHash, valueHash, err
}

func getPrivateKeyPath() string {
	homedir, _ := os.UserHomeDir()
	return filepath.Join(homedir, "iden3/issuer/private.key")
}

func getAuthClaimSchemaHash() (core.SchemaHash, error) {
	return core.NewSchemaHashFromHex("ca938857241db9451ea329256b9c06e5")
}
