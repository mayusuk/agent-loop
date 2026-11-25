//go:build darwin && cgo

package secrets

/*
#cgo CFLAGS: -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>
#include <string.h>

// The file-keychain APIs (SecKeychain*, SecAccess*) are deprecated, but they
// are the only ones that set a per-application access list on an item.

static OSStatus al_open(const char *path, SecKeychainRef *kc) {
	*kc = NULL;
	if (path == NULL || path[0] == 0) return errSecSuccess; // default keychain
	return SecKeychainOpen(path, kc);
}

static void al_close(SecKeychainRef kc) { if (kc) CFRelease(kc); }

static OSStatus al_find(SecKeychainRef kc, const char *svc, const char *acct, UInt32 *len, void **data, SecKeychainItemRef *item) {
	return SecKeychainFindGenericPassword(kc, (UInt32)strlen(svc), svc, (UInt32)strlen(acct), acct, len, data, item);
}

static void al_free(void *data) { SecKeychainItemFreeContent(NULL, data); }

static OSStatus al_delete(SecKeychainRef kc, const char *svc, const char *acct) {
	SecKeychainItemRef item = NULL;
	OSStatus st = al_find(kc, svc, acct, NULL, NULL, &item);
	if (st != errSecSuccess) return st;
	st = SecKeychainItemDelete(item);
	CFRelease(item);
	return st;
}

static OSStatus al_set(SecKeychainRef kc, const char *svc, const char *acct, const char *label,
		const void *data, UInt32 len, const char *app) {
	OSStatus st = al_delete(kc, svc, acct);
	if (st != errSecSuccess && st != errSecItemNotFound) return st;

	SecTrustedApplicationRef ta = NULL;
	st = SecTrustedApplicationCreateFromPath(app, &ta);
	if (st != errSecSuccess) return st;
	CFArrayRef apps = CFArrayCreate(NULL, (const void **)&ta, 1, &kCFTypeArrayCallBacks);
	CFStringRef desc = CFStringCreateWithCString(NULL, label, kCFStringEncodingUTF8);
	SecAccessRef access = NULL;
	st = SecAccessCreate(desc, apps, &access);
	CFRelease(desc);
	CFRelease(apps);
	CFRelease(ta);
	if (st != errSecSuccess) return st;

	SecKeychainAttribute attrs[3] = {
		{ kSecServiceItemAttr, (UInt32)strlen(svc), (void *)svc },
		{ kSecAccountItemAttr, (UInt32)strlen(acct), (void *)acct },
		{ kSecLabelItemAttr, (UInt32)strlen(label), (void *)label },
	};
	SecKeychainAttributeList list = { 3, attrs };
	SecKeychainItemRef item = NULL;
	st = SecKeychainItemCreateFromContent(kSecGenericPasswordItemClass, &list, len, data, kc, access, &item);
	CFRelease(access);
	if (item) CFRelease(item);
	return st;
}

// al_trusted lists the applications that may decrypt the item, one path per
// line in *out (malloc'd). *any is set when an access list entry allows
// every application.
static OSStatus al_trusted(SecKeychainRef kc, const char *svc, const char *acct, char **out, int *any) {
	*out = NULL;
	*any = 0;
	SecKeychainItemRef item = NULL;
	OSStatus st = al_find(kc, svc, acct, NULL, NULL, &item);
	if (st != errSecSuccess) return st;
	SecAccessRef access = NULL;
	st = SecKeychainItemCopyAccess(item, &access);
	CFRelease(item);
	if (st != errSecSuccess) return st;
	CFArrayRef acls = NULL;
	st = SecAccessCopyACLList(access, &acls);
	CFRelease(access);
	if (st != errSecSuccess) return st;

	size_t cap = 1024, used = 0;
	char *buf = malloc(cap);
	buf[0] = 0;
	for (CFIndex i = 0; i < CFArrayGetCount(acls); i++) {
		SecACLRef acl = (SecACLRef)CFArrayGetValueAtIndex(acls, i);
		CFArrayRef auths = SecACLCopyAuthorizations(acl);
		Boolean decrypt = auths && CFArrayContainsValue(auths, CFRangeMake(0, CFArrayGetCount(auths)), kSecACLAuthorizationDecrypt);
		if (auths) CFRelease(auths);
		if (!decrypt) continue;
		CFArrayRef apps = NULL;
		CFStringRef desc = NULL;
		SecKeychainPromptSelector prompt = 0;
		st = SecACLCopyContents(acl, &apps, &desc, &prompt);
		if (desc) CFRelease(desc);
		if (st != errSecSuccess) break;
		if (apps == NULL) { *any = 1; continue; }
		for (CFIndex j = 0; j < CFArrayGetCount(apps); j++) {
			CFDataRef d = NULL;
			if (SecTrustedApplicationCopyData((SecTrustedApplicationRef)CFArrayGetValueAtIndex(apps, j), &d) != errSecSuccess || d == NULL) continue;
			size_t n = strnlen((const char *)CFDataGetBytePtr(d), (size_t)CFDataGetLength(d));
			while (used + n + 2 > cap) { cap *= 2; buf = realloc(buf, cap); }
			memcpy(buf + used, CFDataGetBytePtr(d), n);
			used += n;
			buf[used++] = '\n';
			buf[used] = 0;
			CFRelease(d);
		}
		CFRelease(apps);
	}
	CFRelease(acls);
	*out = buf;
	return st;
}

// al_probe reads the item with user interaction off, so it fails instead of
// asking when the running binary isn't on the item's access list. The value
// is wiped and dropped. It returns 1 when the keychain is locked, since a
// locked keychain fails the same way.
static OSStatus al_probe(SecKeychainRef kc, const char *svc, const char *acct) {
	SecKeychainRef k = kc;
	if (k == NULL) {
		OSStatus st = SecKeychainCopyDefault(&k);
		if (st != errSecSuccess) return st;
	} else {
		CFRetain(k);
	}
	SecKeychainStatus ks = 0;
	OSStatus st = SecKeychainGetStatus(k, &ks);
	if (st == errSecNoSuchKeychain) st = errSecItemNotFound;
	else if (st == errSecSuccess && !(ks & kSecUnlockStateStatus)) st = 1;
	if (st == errSecSuccess) {
		Boolean was = true;
		SecKeychainGetUserInteractionAllowed(&was);
		SecKeychainSetUserInteractionAllowed(false);
		UInt32 len = 0;
		void *data = NULL;
		st = al_find(k, svc, acct, &len, &data, NULL);
		if (data) { memset(data, 0, len); al_free(data); }
		SecKeychainSetUserInteractionAllowed(was);
	}
	CFRelease(k);
	return st;
}

static OSStatus al_create(const char *path, const char *pw, SecKeychainRef *kc) {
	return SecKeychainCreate(path, (UInt32)strlen(pw), pw, false, NULL, kc);
}

static OSStatus al_remove(SecKeychainRef kc) { return SecKeychainDelete(kc); }

static OSStatus al_interaction(int allowed) { return SecKeychainSetUserInteractionAllowed(allowed ? true : false); }

static void al_message(OSStatus st, char *buf, CFIndex len) {
	buf[0] = 0;
	CFStringRef s = SecCopyErrorMessageString(st, NULL);
	if (s == NULL) return;
	CFStringGetCString(s, buf, len, kCFStringEncodingUTF8);
	CFRelease(s);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"strings"
	"unsafe"
)

func osError(st C.OSStatus) error { return osStatusError(int(st)) }

// osStatusError describes a Security framework status.
func osStatusError(st int) error {
	var buf [256]C.char
	C.al_message(C.OSStatus(st), &buf[0], C.CFIndex(len(buf)))
	if msg := C.GoString(&buf[0]); msg != "" {
		return fmt.Errorf("%s (OSStatus %d)", msg, st)
	}
	return fmt.Errorf("OSStatus %d", st)
}

// withKeychain opens the keychain at path (empty: the default) for f.
func withKeychain(path string, f func(C.SecKeychainRef) error) error {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	var kc C.SecKeychainRef
	if st := C.al_open(cpath, &kc); st != C.errSecSuccess {
		return osError(st)
	}
	defer C.al_close(kc)
	return f(kc)
}

func keychainGet(path, service, account string) (v string, found bool, err error) {
	err = withKeychain(path, func(kc C.SecKeychainRef) error {
		svc, acct := C.CString(service), C.CString(account)
		defer C.free(unsafe.Pointer(svc))
		defer C.free(unsafe.Pointer(acct))
		var n C.UInt32
		var data unsafe.Pointer
		st := C.al_find(kc, svc, acct, &n, &data, nil)
		if st == C.errSecItemNotFound {
			return nil
		}
		if st != C.errSecSuccess {
			return osError(st)
		}
		defer C.al_free(data)
		v, found = C.GoStringN((*C.char)(data), C.int(n)), true
		return nil
	})
	return v, found, err
}

func keychainSet(path, service, account, label, value, app string) error {
	return withKeychain(path, func(kc C.SecKeychainRef) error {
		svc, acct, lbl, capp := C.CString(service), C.CString(account), C.CString(label), C.CString(app)
		val := C.CBytes([]byte(value))
		defer C.free(unsafe.Pointer(svc))
		defer C.free(unsafe.Pointer(acct))
		defer C.free(unsafe.Pointer(lbl))
		defer C.free(unsafe.Pointer(capp))
		defer C.free(val)
		if st := C.al_set(kc, svc, acct, lbl, val, C.UInt32(len(value)), capp); st != C.errSecSuccess {
			return osError(st)
		}
		return nil
	})
}

func keychainDelete(path, service, account string) error {
	return withKeychain(path, func(kc C.SecKeychainRef) error {
		svc, acct := C.CString(service), C.CString(account)
		defer C.free(unsafe.Pointer(svc))
		defer C.free(unsafe.Pointer(acct))
		if st := C.al_delete(kc, svc, acct); st != C.errSecSuccess && st != C.errSecItemNotFound {
			return osError(st)
		}
		return nil
	})
}

// keychainReadable reports whether the item exists and whether the running
// binary can read it without a prompt (work plan #95 D7).
func keychainReadable(path, service, account string) (found, readable bool, err error) {
	err = withKeychain(path, func(kc C.SecKeychainRef) error {
		svc, acct := C.CString(service), C.CString(account)
		defer C.free(unsafe.Pointer(svc))
		defer C.free(unsafe.Pointer(acct))
		switch st := C.al_probe(kc, svc, acct); st {
		case C.errSecSuccess:
			found, readable = true, true
		case C.errSecItemNotFound:
		case C.errSecAuthFailed, C.errSecInteractionNotAllowed, C.errSecInteractionRequired:
			found = true
		case 1:
			return errors.New("the keychain is locked")
		default:
			return osError(st)
		}
		return nil
	})
	return found, readable, err
}

// keychainTrusted returns the applications that may read the item without
// a prompt, and whether any application may. Used by tests.
func keychainTrusted(path, service, account string) (apps []string, any bool, err error) {
	err = withKeychain(path, func(kc C.SecKeychainRef) error {
		svc, acct := C.CString(service), C.CString(account)
		defer C.free(unsafe.Pointer(svc))
		defer C.free(unsafe.Pointer(acct))
		var out *C.char
		var cany C.int
		st := C.al_trusted(kc, svc, acct, &out, &cany)
		if out != nil {
			defer C.free(unsafe.Pointer(out))
			apps = strings.Fields(C.GoString(out))
		}
		any = cany != 0
		if st != C.errSecSuccess {
			return osError(st)
		}
		return nil
	})
	return apps, any, err
}

// createKeychain creates an unlocked keychain file. Used by tests.
func createKeychain(path, password string) error {
	cpath, cpw := C.CString(path), C.CString(password)
	defer C.free(unsafe.Pointer(cpath))
	defer C.free(unsafe.Pointer(cpw))
	var kc C.SecKeychainRef
	if st := C.al_create(cpath, cpw, &kc); st != C.errSecSuccess {
		return osError(st)
	}
	C.al_close(kc)
	return nil
}

// removeKeychain deletes a keychain file. Used by tests.
func removeKeychain(path string) error {
	return withKeychain(path, func(kc C.SecKeychainRef) error {
		if st := C.al_remove(kc); st != C.errSecSuccess {
			return osError(st)
		}
		return nil
	})
}

// setKeychainInteraction turns Keychain prompts on or off for the process.
// Tests turn them off, so a missing access-list entry fails instead of
// waiting for a click.
func setKeychainInteraction(allowed bool) error {
	a := 0
	if allowed {
		a = 1
	}
	if st := C.al_interaction(C.int(a)); st != C.errSecSuccess {
		return osError(st)
	}
	return nil
}
