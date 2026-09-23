import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { apiClient } from '../client';
import { logger } from '@/lib/logger';

/**
 * 代理配置（与后端 model.Proxy 对齐）
 */
export interface Proxy {
    id: number;
    name: string;
    url: string;
    remark: string;
    created_at: string;
    updated_at: string;
}

export interface CreateProxyRequest {
    name: string;
    url: string;
    remark?: string;
}

export interface UpdateProxyRequest {
    id: number;
    name?: string;
    url?: string;
    remark?: string;
}

export interface ProxyTestResult {
    success: boolean;
    latency_ms: number;
    ip?: string;
    country?: string;
    error?: string;
}

/**
 * 获取代理列表 Hook
 */
export function useProxyList() {
    return useQuery({
        queryKey: ['proxies', 'list'],
        queryFn: async () => {
            return apiClient.get<Proxy[]>('/api/v1/proxy/list');
        },
        refetchOnMount: 'always',
    });
}

/**
 * 创建代理 Hook
 */
export function useCreateProxy() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: async (data: CreateProxyRequest) => {
            return apiClient.post<Proxy>('/api/v1/proxy/create', data);
        },
        onSuccess: (data) => {
            logger.log('代理创建成功:', data);
            queryClient.invalidateQueries({ queryKey: ['proxies', 'list'] });
        },
        onError: (error) => {
            logger.error('代理创建失败:', error);
        },
    });
}

/**
 * 更新代理 Hook
 */
export function useUpdateProxy() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: async (data: UpdateProxyRequest) => {
            return apiClient.post<Proxy>('/api/v1/proxy/update', data);
        },
        onSuccess: (data) => {
            logger.log('代理更新成功:', data);
            queryClient.invalidateQueries({ queryKey: ['proxies', 'list'] });
        },
        onError: (error) => {
            logger.error('代理更新失败:', error);
        },
    });
}

/**
 * 删除代理 Hook
 */
export function useDeleteProxy() {
    const queryClient = useQueryClient();

    return useMutation({
        mutationFn: async (id: number) => {
            return apiClient.delete<null>(`/api/v1/proxy/delete/${id}`);
        },
        onSuccess: () => {
            logger.log('代理删除成功');
            queryClient.invalidateQueries({ queryKey: ['proxies', 'list'] });
        },
        onError: (error) => {
            logger.error('代理删除失败:', error);
        },
    });
}

/**
 * 测试代理连通性并查询出口 IP Hook
 */
export function useTestProxy() {
    return useMutation({
        mutationFn: async (id: number) => {
            return apiClient.post<ProxyTestResult>('/api/v1/proxy/test', { id });
        },
        onError: (error) => {
            logger.error('代理测试失败:', error);
        },
    });
}
