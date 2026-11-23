/* eta-rwf-dec-oracle.c —— 用 ETA C（librssl.so）解码一条 raw RWF 消息，打印消息头与
 * 字段条目（payload hex + 关键字段的 C 解码结果）。
 *
 * 目的：给纯 Go 的 rwf 解码器提供**可与 C 逐项对照**的参照输出，
 * 让 testdata/rwf_frames.txt 的期望值可复跑复核（对应 rwf/rwf_test.go 的断言）。
 *
 * 用法：
 *   ./eta-rwf-dec-oracle <hex>
 *
 * 编译（Linux，需 RTSDK 头文件与 librssl.so）：
 *   INC=<Real-Time-SDK>/Cpp-C/Eta/Include
 *   gcc -DLinux -o /tmp/eta_rwf_dec_oracle eta-rwf-dec-oracle.c \
 *     -I"$INC/Util" -I"$INC/Codec" -I"$INC/Reactor" -I"$INC/Transport" \
 *     -I"$INC/RDM" -I"$INC/Cache" -I"$INC/Converter" -I"$INC/ansi" \
 *     -L<librssl.so 所在目录> -lrssl
 *   LD_LIBRARY_PATH=<该目录> /tmp/eta_rwf_dec_oracle <hex>
 *
 * 本程序不入构建，只用于生成/复核测试期望值。
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <rtr/rsslTransport.h>
#include <rtr/rsslMsg.h>
#include <rtr/rsslIterators.h>
#include <rtr/rsslDataPackage.h>
#include <rtr/rsslMsgDecoders.h>
#include <rtr/rsslPrimitiveDecoders.h>

static int hexval(char c)
{
    if (c >= '0' && c <= '9') return c - '0';
    if (c >= 'a' && c <= 'f') return c - 'a' + 10;
    if (c >= 'A' && c <= 'F') return c - 'A' + 10;
    return -1;
}

static void dump_hex(const char *p, RsslUInt32 n)
{
    for (RsslUInt32 i = 0; i < n; i++) printf("%02x", (unsigned char)p[i]);
}

int main(int argc, char **argv)
{
    if (argc < 2) {
        fprintf(stderr, "usage: %s <hex>\n", argv[0]);
        return 2;
    }
    const char *hexs = argv[1];
    size_t n = strlen(hexs) / 2;
    char *buf = (char *)malloc(n ? n : 1);
    for (size_t i = 0; i < n; i++) {
        int hi = hexval(hexs[2 * i]), lo = hexval(hexs[2 * i + 1]);
        if (hi < 0 || lo < 0) { fprintf(stderr, "bad hex\n"); return 2; }
        buf[i] = (char)((hi << 4) | lo);
    }

    RsslError error;
    if (rsslInitialize(RSSL_LOCK_NONE, &error) != RSSL_RET_SUCCESS) {
        printf("rsslInitialize failed: %s\n", error.text);
        return 1;
    }

    RsslBuffer rbuf;
    rbuf.data = buf;
    rbuf.length = (RsslUInt32)n;

    RsslDecodeIterator it;
    rsslClearDecodeIterator(&it);
    rsslSetDecodeIteratorRWFVersion(&it, 14, 1);
    rsslSetDecodeIteratorBuffer(&it, &rbuf);

    RsslMsg msg;
    RsslRet ret = rsslDecodeMsg(&it, &msg);
    if (ret != RSSL_RET_SUCCESS) {
        printf("decode msg failed ret=%d (%s)\n", ret, rsslRetCodeToString(ret));
        return 1;
    }
    printf("MSG class=%u domain=%u stream=%d container=%u\n",
           msg.msgBase.msgClass, msg.msgBase.domainType,
           msg.msgBase.streamId, msg.msgBase.containerType);
    printf("FLAGS=0x%x KEY flags=0x%x serviceId=%u name=%.*s nameType=%u\n",
           (msg.msgBase.msgClass == RSSL_MC_REFRESH) ? msg.refreshMsg.flags :
           (msg.msgBase.msgClass == RSSL_MC_UPDATE) ? msg.updateMsg.flags : 0,
           msg.msgBase.msgKey.flags, msg.msgBase.msgKey.serviceId,
           (int)msg.msgBase.msgKey.name.length,
           msg.msgBase.msgKey.name.data ? msg.msgBase.msgKey.name.data : "",
           msg.msgBase.msgKey.nameType);
    if (msg.msgBase.msgClass == RSSL_MC_REFRESH) {
        printf("STATE stream=%u data=%u code=%u text=%.*s\n",
               msg.refreshMsg.state.streamState, msg.refreshMsg.state.dataState,
               msg.refreshMsg.state.code,
               (int)msg.refreshMsg.state.text.length,
               msg.refreshMsg.state.text.data ? msg.refreshMsg.state.text.data : "");
    }
    if (msg.msgBase.msgClass == RSSL_MC_UPDATE)
        printf("SEQNUM=%u\n", msg.updateMsg.seqNum);

    if (msg.msgBase.containerType != RSSL_DT_FIELD_LIST) {
        printf("no field list (container=%u)\n", msg.msgBase.containerType);
        rsslUninitialize();
        return 0;
    }

    RsslFieldList fl;
    rsslClearFieldList(&fl);
    ret = rsslDecodeFieldList(&it, &fl, NULL);
    if (ret != RSSL_RET_SUCCESS && ret != RSSL_RET_SET_SKIPPED) {
        printf("decode field list failed ret=%d\n", ret);
        rsslUninitialize();
        return 1;
    }
    printf("FL flags=0x%x dictId=%d listNum=%d\n", fl.flags, fl.dictionaryId, fl.fieldListNum);

    RsslFieldEntry fe;
    int count = 0;
    while ((ret = rsslDecodeFieldEntry(&it, &fe)) != RSSL_RET_END_OF_CONTAINER) {
        if (ret != RSSL_RET_SUCCESS) {
            printf("entry decode failed ret=%d\n", ret);
            break;
        }
        count++;
        /* 只打印关心字段，避免刷屏 */
        if (fe.fieldId == 3 || fe.fieldId == 5 || fe.fieldId == 17 ||
            fe.fieldId == 22 || fe.fieldId == 25 || fe.fieldId == 134 || fe.fieldId == 3855) {
            printf("FIELD fid=%u type=%u hex=", fe.fieldId, fe.dataType);
            dump_hex(fe.encData.data, fe.encData.length);
            switch (fe.fieldId) {
                case 3: {
                    RsslBuffer b; b.data = 0; b.length = 0;
                    if (rsslDecodeBuffer(&it, &b) >= 0 && b.length)
                        printf(" value=%.*s", (int)b.length, b.data);
                    break;
                }
                case 22: case 25: case 134: {
                    RsslReal r; rsslClearReal(&r);
                    if (rsslDecodeReal(&it, &r) >= 0) {
                        char out[64]; RsslBuffer ob; ob.data = out; ob.length = sizeof(out);
                        if (rsslRealToString(&ob, &r) == RSSL_RET_SUCCESS)
                            printf(" hint=%u value=%lld str=%.*s", r.hint, (long long)r.value,
                                   (int)ob.length, ob.data);
                    }
                    break;
                }
                case 5: {
                    RsslTime t; rsslClearTime(&t);
                    if (rsslDecodeTime(&it, &t) >= 0)
                        printf(" time=%02u:%02u:%02u", t.hour, t.minute, t.second);
                    break;
                }
                case 17: {
                    RsslDate d; rsslClearDate(&d);
                    if (rsslDecodeDate(&it, &d) >= 0)
                        printf(" date=%04u-%02u-%02u", d.year, d.month, d.day);
                    break;
                }
                case 3855: {
                    RsslUInt64 v = 0;
                    if (rsslDecodeUInt(&it, &v) >= 0)
                        printf(" uint=%llu", (unsigned long long)v);
                    break;
                }
            }
            printf("\n");
        }
    }
    printf("ENTRIES=%d\n", count);
    rsslUninitialize();
    return 0;
}
