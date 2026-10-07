package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// StorageClass is the Cloud Storage class an object is held in. A recording
// bucket moves objects to colder classes as they age, and each class has its
// own price.
type StorageClass string

const (
	StorageStandard StorageClass = "standard"
	StorageNearline StorageClass = "nearline"
	StorageColdline StorageClass = "coldline"
	StorageArchive  StorageClass = "archive"
)

// StoragePrices is what keeping one GiB for one month costs, by class.
type StoragePrices map[StorageClass]Micros

// ProductRecordingStorage marks a usage entry as hosted recording storage
// rather than a participant's session.
const ProductRecordingStorage = "recording_storage"

// hoursPerMonth is the month Cloud Storage prices in: its per-GiB-hour rates
// are the monthly ones divided by 730.
const hoursPerMonth = 730

// GCSStorageListPrice is Cloud Storage's at-rest list price for a bucket
// location, in USD per GiB-month. ok is false for a location this table does
// not know, which must be added rather than guessed.
//
// Source: https://cloud.google.com/storage/pricing ("Data storage"), retrieved
// 7 October 2026, where the rates are given per GiB-hour: regional Standard
// $0.000027397, Nearline $0.000013699, Coldline $0.000005479, Archive
// $0.000001644; multi-region Standard $0.000035616, Nearline $0.000020548,
// Coldline $0.000011986, Archive $0.00000411. Times 730 hours, rounded to the
// micro-dollar. The regional figures are for the North American regions
// MOQOM uses (us-west1 is dev's); other regions differ and are not listed.
// Excluded, and so not billed: operation charges, retrieval and early-deletion
// fees for the colder classes, and inter-region replication on writes.
// Recheck when Google changes theirs; nothing here does it automatically.
func GCSStorageListPrice(location string) (StoragePrices, bool) {
	switch strings.ToLower(strings.TrimSpace(location)) {
	case "us-west1", "us-central1", "us-east1":
		return StoragePrices{
			StorageStandard: 20_000, // $0.020
			StorageNearline: 10_000, // $0.010
			StorageColdline: 4_000,  // $0.004
			StorageArchive:  1_200,  // $0.0012
		}, true
	case "us":
		return StoragePrices{
			StorageStandard: 26_000, // $0.026
			StorageNearline: 15_000, // $0.015
			StorageColdline: 8_750,  // $0.00875
			StorageArchive:  3_000,  // $0.003
		}, true
	}
	return nil, false
}

// Largest single storage record: a day of the largest recording anyone could
// plausibly hold. The product of the two must fit an int64, which these do
// with room to spare (2^46 bytes for 2^17 seconds).
const (
	longestStorageInterval = 24 * 60 * 60
	largestStoredBytes     = int64(1) << 46 // 64 TiB
)

// RateStorage prices bytes held in one class for some seconds: cost at the
// configured price, plus the same markup as relaying. No platform fee and no
// ceiling: Agora has no line for this to be capped against.
func (p Pricing) RateStorage(class StorageClass, bytes, seconds int64) Charge {
	rate := p.Costs.Storage[class]
	if bytes <= 0 || seconds <= 0 || rate <= 0 {
		return Charge{}
	}
	cost := scale(rate, bytes*seconds, gibibyte*hoursPerMonth*3600)
	return Charge{Cost: cost, Price: cost + scale(cost, p.MarkupBasisPoints, 10_000)}
}

// StorageRecord is the bytes one recording held in one storage class over
// one interval, as the recording meter reports it. ID is unique per
// recording, attempt, day and class, and is what makes a retried report
// charge once.
type StorageRecord struct {
	ID        string
	Tenant    string
	Room      string
	Recording string
	Class     StorageClass
	Bytes     int64
	Seconds   int64
}

// RecordStorage rates and charges recording storage through the same ledger
// as relayed usage, so it counts against the balance, the spend limit and
// the alerts, and appears in the usage summary.
func (s *Service) RecordStorage(ctx context.Context, records []StorageRecord) (UsageResult, error) {
	var result UsageResult
	touched := map[string]string{}
	for i, record := range records {
		if record.ID == "" || len(record.ID) > 200 {
			return result, fmt.Errorf("%w: storage record %d needs an id of at most 200 characters", ErrInvalid, i)
		}
		if _, ok := s.pricing.Costs.Storage[record.Class]; !ok {
			return result, fmt.Errorf("%w: storage record %s has an unpriced class %q", ErrInvalid, record.ID, record.Class)
		}
		if record.Tenant == "" {
			return result, fmt.Errorf("%w: storage record %s names no tenant", ErrInvalid, record.ID)
		}
		if record.Bytes < 0 || record.Bytes > largestStoredBytes ||
			record.Seconds < 0 || record.Seconds > longestStorageInterval {
			return result, fmt.Errorf("%w: storage record %s has a negative or implausible quantity", ErrInvalid, record.ID)
		}
		if s.exempt[record.Tenant] {
			result.Unbilled++
			continue
		}
		if _, err := s.store.GetTenant(ctx, record.Tenant); err != nil {
			if errors.Is(err, ErrNotFound) {
				s.log.WarnContext(ctx, "storage for an unregistered tenant", "tenant", record.Tenant, "record", record.ID)
				result.Unbilled++
				continue
			}
			return result, err
		}

		charge := s.pricing.RateStorage(record.Class, record.Bytes, record.Seconds)
		id, err := s.randomHex(12)
		if err != nil {
			return result, err
		}
		key := "storage:" + record.Tenant + ":" + record.ID
		detail, _ := json.Marshal(map[string]any{
			"product":            ProductRecordingStorage,
			"hosting":            HostingCloud,
			"room":               record.Room,
			"recording":          record.Recording,
			"storage_class":      record.Class,
			"storage_bytes":      record.Bytes,
			"storage_seconds":    record.Seconds,
			"storage_byte_hours": byteHours(record.Bytes, record.Seconds),
		})
		_, created, err := s.store.AppendEntry(ctx, Entry{
			ID:             id,
			Tenant:         record.Tenant,
			Kind:           KindUsage,
			Amount:         -charge.Price,
			Cost:           charge.Cost,
			IdempotencyKey: key,
			Reference:      record.Recording,
			Detail:         string(detail),
			At:             s.now(),
		})
		if err != nil {
			return result, err
		}
		if !created {
			result.Duplicates++
			continue
		}
		result.Charged++
		result.Total += charge.Price
		touched[record.Tenant] = key
	}
	for tenant, trigger := range touched {
		s.checkSpendAlerts(ctx, tenant)
		s.maybeTopUp(ctx, tenant, trigger)
	}
	return result, nil
}

// byteHours rounds half up. Reporting only; the charge is on byte-seconds.
func byteHours(bytes, seconds int64) int64 {
	return int64(scale(Micros(bytes), seconds, 3600))
}

// GiBMonths is byte-hours as the unit storage is priced in.
func GiBMonths(byteHours int64) float64 {
	return float64(byteHours) / gibibyte / hoursPerMonth
}
