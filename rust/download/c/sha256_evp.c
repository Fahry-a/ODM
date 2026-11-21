#include <stddef.h>

/* Opaque declarations avoid OpenSSL's include path: openssl-sys already
   links the vendored libcrypto into this crate. */
typedef struct evp_md_ctx_st EVP_MD_CTX;
typedef struct evp_md_st EVP_MD;

extern const EVP_MD *EVP_sha256(void);
extern EVP_MD_CTX *EVP_MD_CTX_new(void);
extern void EVP_MD_CTX_free(EVP_MD_CTX *ctx);
extern int EVP_DigestInit_ex(EVP_MD_CTX *ctx, const EVP_MD *type, void *impl);
extern int EVP_DigestUpdate(EVP_MD_CTX *ctx, const void *data, size_t len);
extern int EVP_DigestFinal_ex(EVP_MD_CTX *ctx, unsigned char *md, unsigned int *len);

void *odm_sha256_new(void) {
    EVP_MD_CTX *ctx = EVP_MD_CTX_new();
    if (ctx == NULL || EVP_DigestInit_ex(ctx, EVP_sha256(), NULL) != 1) {
        EVP_MD_CTX_free(ctx);
        return NULL;
    }
    return ctx;
}

void odm_sha256_update(void *p, const unsigned char *data, size_t len) {
    if (len != 0) {
        EVP_DigestUpdate((EVP_MD_CTX *)p, data, len);
    }
}

void odm_sha256_final(void *p, unsigned char *out) {
    unsigned int len = 0;
    EVP_DigestFinal_ex((EVP_MD_CTX *)p, out, &len);
}

void odm_sha256_free(void *p) {
    EVP_MD_CTX_free((EVP_MD_CTX *)p);
}
