/* eta-rwf-enc-oracle.c —— 用 ETA C（librssl.so）编码 raw REQUEST/CLOSE 消息，打印 hex。
 *
 * 目的：给纯 Go 的 rwf/encode（rwf/encode）提供**逐字节黄金基准**，
 * 让「编码必须与 C 完全一致」变成可复跑的门禁（基准值记录在 rwf/encode_test.go）。
 *
 * 原仓（转译来源）：https://github.com/Refinitiv/Real-Time-SDK @ 7d219a600（Apache-2.0，LSEG/Refinitiv）
 *
 * 编译（Linux，需 RTSDK 头文件与 librssl.so）：
 *   INC=<Real-Time-SDK>/Cpp-C/Eta/Include
 *   gcc -DLinux -o /tmp/eta_enc_oracle eta-rwf-enc-oracle.c \
 *     -I"$INC/Util" -I"$INC/Codec" -I"$INC/Reactor" -I"$INC/Transport" \
 *     -I"$INC/RDM" -I"$INC/Cache" -I"$INC/Converter" -I"$INC/ansi" \
 *     -L<librssl.so 所在目录> -lrssl
 *   LD_LIBRARY_PATH=<该目录> /tmp/eta_enc_oracle
 *
 * 输出（ETAC 3.10.1；参数为占位值 user=username / serviceId=999 / name=TEST，
 * 与 rwf/encode_test.go 的常量一一对应）：
 *   LOGIN   len=23 00150101000000010400800b0608757365726e616d6501
 *   ITEM999 len=22 00140106000000030400800a07fe03e7045445535401
 *   ITEM100 len=20 0012010600000003040080080764045445535401
 *   ITEM254 len=22 00140106000000030400800a07fe00fe045445535401
 *   CLOSE   len=10 00080506000000030000
 *
 * 换 SDK 版本后重跑本程序并更新 rwf/encode_test.go 常量即可（本程序不入构建）。
 */
#include <stdio.h>
#include <string.h>

#include <rtr/rsslTransport.h>
#include <rtr/rsslMsg.h>
#include <rtr/rsslIterators.h>
#include <rtr/rsslDataPackage.h>
#include <rtr/rsslMsgEncoders.h>

static void dump(const char *name, const unsigned char *buf, RsslUInt32 len)
{
    printf("%s len=%u hex=", name, len);
    for (RsslUInt32 i = 0; i < len; i++) printf("%02x", buf[i]);
    printf("\n");
}

static int encode_request(RsslUInt32 major, RsslUInt32 minor,
                          RsslInt32 streamId, RsslUInt8 domain,
                          RsslUInt16 keyFlags, RsslUInt16 serviceId,
                          RsslUInt8 nameType, const char *name,
                          RsslUInt16 requestFlags,
                          unsigned char *out, RsslUInt32 outCap, RsslUInt32 *outLen)
{
    unsigned char buf[4096];
    RsslBuffer rbuf;
    RsslEncodeIterator it;
    RsslMsg msg;
    RsslRet ret;

    rbuf.data = (char *)buf;
    rbuf.length = sizeof(buf);

    rsslClearEncodeIterator(&it);
    rsslSetEncodeIteratorBuffer(&it, &rbuf);
    rsslSetEncodeIteratorRWFVersion(&it, major, minor);

    rsslClearMsg(&msg);
    msg.msgBase.msgClass = RSSL_MC_REQUEST;
    msg.msgBase.domainType = domain;
    msg.msgBase.containerType = RSSL_DT_NO_DATA;
    msg.msgBase.streamId = streamId;
    msg.msgBase.msgKey.flags = keyFlags;
    msg.msgBase.msgKey.serviceId = serviceId;
    msg.msgBase.msgKey.nameType = nameType;
    msg.msgBase.msgKey.name.length = (RsslUInt32)strlen(name);
    msg.msgBase.msgKey.name.data = (char *)name;
    msg.requestMsg.flags = requestFlags;

    ret = rsslEncodeMsg(&it, &msg);
    if (ret != RSSL_RET_SUCCESS) {
        printf("encode failed ret=%d (%s)\n", ret, rsslRetCodeToString(ret));
        return -1;
    }
    *outLen = (RsslUInt32)((unsigned char *)it._curBufPtr - buf);
    if (*outLen > outCap) return -1;
    memcpy(out, buf, *outLen);
    return 0;
}

int main(void)
{
    unsigned char out[4096];
    RsslUInt32 len = 0;
    RsslError error;
    RsslRet ret;

    ret = rsslInitialize(RSSL_LOCK_NONE, &error);
    if (ret != RSSL_RET_SUCCESS) {
        printf("rsslInitialize failed: %s\n", error.text);
        return 1;
    }

    /* 1) raw login：stream=1 domain=LOGIN(1) name=username nameType=1 flags=STREAMING */
    if (encode_request(14, 1, 1, 1,
                       RSSL_MKF_HAS_NAME | RSSL_MKF_HAS_NAME_TYPE, 0, 1,
                       "username", RSSL_RQMF_STREAMING, out, sizeof(out), &len) == 0)
        dump("LOGIN", out, len);

    /* 2) item request：stream=3 domain=MARKET_PRICE(6) serviceId=999 name=TEST */
    if (encode_request(14, 1, 3, 6,
                       RSSL_MKF_HAS_SERVICE_ID | RSSL_MKF_HAS_NAME | RSSL_MKF_HAS_NAME_TYPE,
                       999, 1, "TEST", RSSL_RQMF_STREAMING, out, sizeof(out), &len) == 0)
        dump("ITEM999", out, len);

    /* 3) item request serviceId=100（<0xFE，单字节形态） */
    if (encode_request(14, 1, 3, 6,
                       RSSL_MKF_HAS_SERVICE_ID | RSSL_MKF_HAS_NAME | RSSL_MKF_HAS_NAME_TYPE,
                       100, 1, "TEST", RSSL_RQMF_STREAMING, out, sizeof(out), &len) == 0)
        dump("ITEM100", out, len);

    /* 4) item request serviceId=254（边界：>=0xFE 走三字节形态） */
    if (encode_request(14, 1, 3, 6,
                       RSSL_MKF_HAS_SERVICE_ID | RSSL_MKF_HAS_NAME | RSSL_MKF_HAS_NAME_TYPE,
                       254, 1, "TEST", RSSL_RQMF_STREAMING, out, sizeof(out), &len) == 0)
        dump("ITEM254", out, len);

    /* 5) close request（stream=3） */
    {
        unsigned char buf[64];
        RsslBuffer rbuf;
        RsslEncodeIterator it;
        RsslMsg msg;
        rbuf.data = (char *)buf;
        rbuf.length = sizeof(buf);
        rsslClearEncodeIterator(&it);
        rsslSetEncodeIteratorBuffer(&it, &rbuf);
        rsslSetEncodeIteratorRWFVersion(&it, 14, 1);
        rsslClearMsg(&msg);
        msg.msgBase.msgClass = RSSL_MC_CLOSE;
        msg.msgBase.domainType = 6;
        msg.msgBase.containerType = RSSL_DT_NO_DATA;
        msg.msgBase.streamId = 3;
        if (rsslEncodeMsg(&it, &msg) == RSSL_RET_SUCCESS)
            dump("CLOSE", buf, (RsslUInt32)((unsigned char *)it._curBufPtr - buf));
    }

    rsslUninitialize();
    return 0;
}
