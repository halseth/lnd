package migtest

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/coreos/bbolt"
)

// DumpDB dumps go code describing the contents of the database to stdout. This
// function is only intended for use during development.
//
// Example output:
//
// map[string]interface{}{
// 	"0x1234": map[string]interface{}{
// 		"human-readable": "0x102030",
// 		"0x1111": "0x5783492373",
// 	},
// }
func DumpDB(tx *bbolt.Tx, rootKey []byte) error {
	bucket := tx.Bucket(rootKey)
	if bucket == nil {
		return fmt.Errorf("bucket %v not found", string(rootKey))
	}

	return dumpBucket(bucket)
}

func dumpBucket(bucket *bbolt.Bucket) error {
	fmt.Printf("map[string]interface{} {\n")
	err := bucket.ForEach(func(k, v []byte) error {
		key := toString(k)
		fmt.Printf("\"%v\": ", key)

		subBucket := bucket.Bucket(k)
		if subBucket != nil {
			err := dumpBucket(subBucket)
			if err != nil {
				return err
			}
		} else {
			fmt.Printf("\"%v\"", toHex(v))
		}
		fmt.Printf(",\n")

		return nil
	})
	if err != nil {
		return err
	}
	fmt.Printf("}")

	return nil
}

// RestoreDB primes the database with the given data set.
func RestoreDB(tx *bbolt.Tx, rootKey []byte, data map[string]interface{}) error {
	bucket, err := tx.CreateBucket(rootKey)
	if err != nil {
		return err
	}

	return restoreDB(bucket, data)
}

func restoreDB(bucket *bbolt.Bucket, data map[string]interface{}) error {
	for k, v := range data {
		key, err := parseDbString(k)
		if err != nil {
			return err
		}

		switch value := v.(type) {

		// Key contains value.
		case string:
			parsedValue, err := parseDbString(value)
			if err != nil {
				return err
			}

			err = bucket.Put(key, parsedValue)
			if err != nil {
				return err
			}

		// Key contains a sub-bucket.
		case map[string]interface{}:
			subBucket, err := bucket.CreateBucket(key)
			if err != nil {
				return err
			}

			if err := restoreDB(subBucket, value); err != nil {
				return err
			}

		default:
			return errors.New("invalid type")
		}
	}

	return nil
}

// VerifyDB verifies the database against the given data set.
func VerifyDB(tx *bbolt.Tx, rootKey []byte, data map[string]interface{}) error {
	bucket := tx.Bucket(rootKey)
	if bucket == nil {
		return fmt.Errorf("bucket %v not found", string(rootKey))
	}

	return verifyDB(bucket, data)
}

func verifyDB(bucket *bbolt.Bucket, data map[string]interface{}) error {
	for k, v := range data {
		key, err := parseDbString(k)
		if err != nil {
			return err
		}

		switch value := v.(type) {

		// Key contains value.
		case string:
			parsedValue, err := parseDbString(value)
			if err != nil {
				return err
			}

			dbValue := bucket.Get(key)

			if !bytes.Equal(dbValue, parsedValue) {
				return errors.New("value mismatch")
			}

		// Key contains a sub-bucket.
		case map[string]interface{}:
			subBucket := bucket.Bucket(key)
			if subBucket == nil {
				return fmt.Errorf("bucket %v not found", k)
			}

			err := verifyDB(subBucket, value)
			if err != nil {
				return err
			}

		default:
			return errors.New("invalid type")
		}
	}

	keyCount := 0
	err := bucket.ForEach(func(k, v []byte) error {
		keyCount++
		return nil
	})
	if err != nil {
		return err
	}
	if keyCount != len(data) {
		return errors.New("unexpected keys in database")
	}

	return nil
}

// parseDbString decodes a database string into a byte slice.
func parseDbString(key string) ([]byte, error) {
	if strings.HasPrefix(key, "0x") {
		return hex.DecodeString(key[2:])
	}

	return []byte(key), nil
}

func toHex(v []byte) string {
	if len(v) == 0 {
		return ""
	}

	return "0x" + hex.EncodeToString(v)
}

func toString(v []byte) string {
	readableChars := "abcdefghijklmnopqrstuvwxyz0123456789-"

	for _, c := range v {
		if !strings.Contains(readableChars, string(c)) {
			return toHex(v)
		}
	}

	return string(v)
}
