'use client';

import { useCallback, useMemo, useState } from 'react';
import { ArrowUp } from 'lucide-react';
import { useLogs } from '@/api/endpoints/log';
import { LogCard } from './Item';
import { Loader2 } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { VirtualizedGrid } from '@/components/common/VirtualizedGrid';

/**
 * 日志页面组件
 * - 初始加载 pageSize 条历史日志
 * - SSE 实时推送先进缓冲区（避免滚动中头部插入打乱虚拟列表）
 * - 顶部按钮合入缓冲日志并回到最新位置
 * - 滚动到底部自动加载更多
 */
export function Log() {
    const t = useTranslations('log');
    const { logs, hasMore, isLoading, isLoadingMore, loadMore, pendingLiveCount, flushLiveLogs } = useLogs({ pageSize: 30 });
    const [scrollSignal, setScrollSignal] = useState(0);

    const canLoadMore = hasMore && !isLoading && !isLoadingMore && logs.length > 0;
    const handleReachEnd = useCallback(() => {
        if (!canLoadMore) return;
        void loadMore();
    }, [canLoadMore, loadMore]);

    const handleFlushLive = useCallback(() => {
        flushLiveLogs();
        setScrollSignal((signal) => signal + 1);
    }, [flushLiveLogs]);

    const footer = useMemo(() => {
        if (hasMore && (isLoading || isLoadingMore)) {
            return (
                <div className="flex justify-center py-4">
                    <Loader2 className="h-6 w-6 animate-spin text-muted-foreground" />
                </div>
            );
        }
        if (!hasMore && logs.length > 0) {
            return (
                <div className="flex justify-center py-4">
                    <span className="text-sm text-muted-foreground">{t('list.noMore')}</span>
                </div>
            );
        }
        return null;
    }, [hasMore, isLoading, isLoadingMore, logs.length, t]);

    return (
        <div className="relative h-full min-h-0 w-full">
            {pendingLiveCount > 0 && (
                <button
                    type="button"
                    onClick={handleFlushLive}
                    className="absolute left-1/2 top-3 z-10 flex -translate-x-1/2 items-center gap-1.5 rounded-full border bg-background/90 px-4 py-1.5 text-xs font-medium shadow-md backdrop-blur transition-colors hover:bg-accent"
                >
                    <ArrowUp className="size-3.5" />
                    {t('list.newLogs', { count: pendingLiveCount })}
                </button>
            )}
            <VirtualizedGrid
                items={logs}
                layout="list"
                columns={{ default: 1 }}
                estimateItemHeight={80}
                overscan={8}
                getItemKey={(log) => `log-${log.id}`}
                renderItem={(log) => <LogCard log={log} />}
                footer={footer}
                onReachEnd={handleReachEnd}
                reachEndEnabled={canLoadMore}
                reachEndOffset={2}
                scrollToTopSignal={scrollSignal}
            />
        </div>
    );
}
